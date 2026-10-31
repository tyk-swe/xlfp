"""Offline checks for generator provenance, corpus consistency, and encoding."""
import contextlib
import hashlib
import importlib.util
import io
import json
import pathlib
import tempfile
import types
import unittest
from unittest import mock


class GeneratorTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        # Loading the generator should not require tokenizer downloads. Tokenizer
        # implementations are deliberately not tested by these offline stubs.
        deps = {
            "tiktoken": types.SimpleNamespace(),
            "huggingface_hub": types.SimpleNamespace(hf_hub_download=None),
            "tokenizers": types.SimpleNamespace(Tokenizer=None),
        }
        path = pathlib.Path(__file__).with_name("gen_db.py")
        spec = importlib.util.spec_from_file_location("gen_db_under_test", path)
        cls.generator = importlib.util.module_from_spec(spec)
        with mock.patch.dict("sys.modules", deps):
            spec.loader.exec_module(cls.generator)

    def test_probe_corpus_matches_embedded_database(self):
        path = pathlib.Path(__file__).resolve().parents[1] / "pkg/fingerprint/db.json"
        db = json.loads(path.read_text(encoding="utf-8"))
        self.assertEqual(self.generator.PROBES, db["probes"])

    def test_download_hash_is_verified(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp) / "tokenizer"
            path.write_bytes(b"fixture tokenizer")
            digest = hashlib.sha256(path.read_bytes()).hexdigest()
            with mock.patch.object(self.generator, "hf_hub_download", return_value=str(path)) as download:
                self.assertEqual(self.generator.pinned_download("repo", "file", "revision", digest), str(path))
                download.assert_called_once_with("repo", "file", revision="revision")
                with self.assertRaises(SystemExit):
                    self.generator.pinned_download("repo", "file", "revision", "0" * 64)

    def test_output_explicitly_uses_utf8(self):
        counts = [1] * len(self.generator.PROBES)
        with mock.patch.object(self.generator, "tiktoken_counts", return_value=counts), \
             mock.patch.object(self.generator, "hf_counts", return_value=counts), \
             mock.patch.object(self.generator, "glm_counts", return_value=counts), \
             mock.patch.object(self.generator, "kimi_counts", return_value=counts), \
             mock.patch.object(pathlib.Path, "write_text", autospec=True) as write, \
             contextlib.redirect_stdout(io.StringIO()):
            self.generator.main()
        write.assert_called_once()
        self.assertEqual(write.call_args.kwargs.get("encoding"), "utf-8")
        self.assertEqual(json.loads(write.call_args.args[1])["probes"], self.generator.PROBES)


if __name__ == "__main__":
    unittest.main()
