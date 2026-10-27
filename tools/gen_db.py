"""Generate pkg/fingerprint/db.json from real tokenizer files.

Run with the project venv:
    .venv/bin/pip install -r tools/requirements.txt
    .venv/bin/python tools/gen_db.py

Every Hugging Face download is pinned to a revision and verified against
a recorded SHA-256 (see NOTICE.md for provenance). Counts are raw-text
token counts (no chat-template tokens). The Go ranking code models a
constant per-endpoint chat-template overhead on top of these.
"""
import hashlib
import json
import pathlib

import tiktoken
from huggingface_hub import hf_hub_download
from tokenizers import Tokenizer

PROBES = [
    {"id": "en_pangram", "stage": 1, "text": "The quick brown fox jumps over the lazy dog. Pack my box with five dozen liquor jugs!"},
    {"id": "cjk_zh", "stage": 1, "text": "机器学习模型需要大量的训练数据和计算资源。深度学习改变了人工智能。"},
    {"id": "numbers", "stage": 1, "text": "1234567890 987654321 3.14159265358979 1000000 42 0.001"},
    {"id": "emoji_zwj", "stage": 1, "text": "😀🚀🧑‍💻🇺🇸👩🏾‍🔬❤️✨🔥"},
    {"id": "code_go", "stage": 2, "text": "func main() { fmt.Println(\"hello, world\") } // x := a + b*2"},
    {"id": "cjk_ja_ko", "stage": 2, "text": "こんにちは世界、これはテストです。안녕하세요 세계, 이것은 테스트입니다."},
    {"id": "whitespace", "stage": 2, "text": "a    b\n\n\nc\td      e        f"},
    {"id": "unicode_mixed", "stage": 2, "text": "Crème brûlée — naïve façade. Привет мир! مرحبا بالعالم"},
    {"id": "repeated_a", "stage": 3, "text": "a" * 64},
    {"id": "en_short", "stage": 3, "text": "Hello world"},
    {"id": "special_a", "stage": 3, "special": True, "text": "<|endoftext|> hello <|im_start|> user"},
    {"id": "special_b", "stage": 3, "special": True, "text": "[INST] <<SYS>> <s> </s> <|begin_of_text|>"},
]

# (repo, filename, pinned revision, sha256 of the file at that revision)
HF = {
    "qwen": ("Qwen/Qwen2.5-7B-Instruct", "tokenizer.json", "a09a35458c702b33eeacc393d103063234e8bc28", "c0382117ea329cdf097041132f6d735924b697924d6f6fc3945713e96ce87539"),
    "deepseek": ("deepseek-ai/DeepSeek-V3", "tokenizer.json", "e815299b0bcbac849fa540c768ef21845365c9eb", "621ac2e32d0dba658404412318818aaa8ce8cda492e59830109d8da6b517fb41"),
    "mistral": ("mistralai/Mistral-7B-Instruct-v0.3", "tokenizer.json", "c170c708c41dac9275d15a8fff4eca08d52bab71", "e553af6fff7d7ad76e830608b218c5c0b0822998d5a1a96099a74cd3c1cb1a49"),
    "mimo": ("XiaomiMiMo/MiMo-7B-Base", "tokenizer.json", "c72df4586cb8bdeebd65f36929cd3385a6566fbe", "c0382117ea329cdf097041132f6d735924b697924d6f6fc3945713e96ce87539"),
    "claude": ("Xenova/claude-tokenizer", "tokenizer.json", "cae688821ea05490de49a6d3faa36468a4672fad", "c241737df24b4e7f7c9af4fdcee29a0ca903dcb288a8b753bc346a3092911767"),
    "google": ("Xenova/gemma-tokenizer", "tokenizer.json", "fbc8c39d11899b177f1fe8f1d25bd25fbee66fad", "05e97791a5e007260de1db7e1692e53150e08cea481e2bf25435553380c147ee"),
}
GLM = ("THUDM/glm-4-9b-chat", "tokenizer.model", "bd8234fe5e0c09c48637a92abb0c797cb5fa0e73", "5a493598071550244b2ee7f26118f3edec2150b9dfa967929a99052ac83fe716")


def pinned_download(repo, fname, revision, sha256):
    path = hf_hub_download(repo, fname, revision=revision)
    digest = hashlib.sha256(pathlib.Path(path).read_bytes()).hexdigest()
    if digest != sha256:
        raise SystemExit(f"sha256 mismatch for {repo}@{revision} {fname}: got {digest}, want {sha256}")
    return path


def hf_counts(spec):
    tok = Tokenizer.from_file(pinned_download(*spec))
    return [len(tok.encode(p["text"], add_special_tokens=False).ids) for p in PROBES]


def tiktoken_counts(name):
    enc = tiktoken.get_encoding(name)
    # disallowed_special=() so special-token literals are encoded as specials
    return [len(enc.encode(p["text"], disallowed_special=())) for p in PROBES]


def glm_counts():
    # GLM-4 ships a tiktoken rank file named tokenizer.model (see
    # tokenization_chatglm.py in THUDM/glm-4-9b-chat).
    import base64

    path = pinned_download(*GLM)
    ranks = {}
    with open(path, encoding="utf-8") as f:
        for line in f:
            parts = line.strip().split()
            if len(parts) == 2:
                ranks[base64.b64decode(parts[0])] = int(parts[1])
    pat = r"(?i:'s|'t|'re|'ve|'m|'ll|'d)|[^\r\n\p{L}\p{N}]?\p{L}+|\p{N}{1,3}| ?[^\s\p{L}\p{N}]+[\r\n]*|\s*[\r\n]+|\s+(?!\S)|\s+"
    enc = tiktoken.Encoding(name="glm4", pat_str=pat, mergeable_ranks=ranks, special_tokens={})
    return [len(enc.encode(p["text"])) for p in PROBES]


def entry(eid, family, display, tokenizer, verified, source, counts, reject=None, aliases=None):
    return {
        "id": eid,
        "family": family,
        "display": display,
        "tokenizer": tokenizer,
        "verified": verified,
        "source": source,
        "counts": counts,
        "reject_probes": reject or [],
        "aliases": aliases or [],
    }


def main():
    entries = [
        entry("openai-o200k", "openai", "OpenAI", "o200k_base", True,
              "tiktoken o200k_base (official encoding)", tiktoken_counts("o200k_base"),
              reject=["special_a"], aliases=["gpt", "gpt-4", "gpt-5", "o1", "o3", "chatgpt"]),
        entry("openai-cl100k", "openai", "OpenAI", "cl100k_base", True,
              "tiktoken cl100k_base (official encoding)", tiktoken_counts("cl100k_base"),
              reject=["special_a"], aliases=["gpt-3.5", "gpt-4-turbo"]),
        entry("qwen25", "qwen", "Qwen", "Qwen2.5 BPE", True,
              "Qwen/Qwen2.5-7B-Instruct tokenizer.json", hf_counts(HF["qwen"]),
              aliases=["qwen", "tongyi", "qwq"]),
        entry("deepseek-v3", "deepseek", "DeepSeek", "DeepSeek V3 BPE", True,
              "deepseek-ai/DeepSeek-V3 tokenizer.json", hf_counts(HF["deepseek"]),
              aliases=["deepseek"]),
        entry("mistral-v3", "mistral", "Mistral", "Mistral v3 Tekken/BPE", True,
              "mistralai/Mistral-7B-Instruct-v0.3 tokenizer.json", hf_counts(HF["mistral"]),
              aliases=["mistral", "mixtral", "codestral"]),
        entry("glm4", "glm", "GLM", "GLM-4 tiktoken-format BPE", True,
              "THUDM/glm-4-9b-chat tokenizer.model", glm_counts(),
              aliases=["glm", "chatglm", "zhipu"]),
        entry("mimo-7b", "mimo", "MiMo", "MiMo-7B BPE", True,
              "XiaomiMiMo/MiMo-7B-Base tokenizer.json", hf_counts(HF["mimo"]),
              aliases=["mimo", "xiaomi"]),
        entry("google-gemma-sp", "google", "Google", "Gemma SentencePiece (Gemini proxy)", False,
              "Xenova/gemma-tokenizer tokenizer.json (community conversion; Gemini server tokenizer is closed)",
              hf_counts(HF["google"]), aliases=["gemini", "gemma", "google"]),
        entry("claude-xenova", "claude", "Claude", "Claude BPE (community conversion)", False,
              "Xenova/claude-tokenizer tokenizer.json (community conversion; current Claude server tokenizer is closed)",
              hf_counts(HF["claude"]), aliases=["claude", "anthropic", "sonnet", "opus", "haiku"]),
    ]
    db = {
        "version": "1.0.0",
        "note": "Raw-text counts; ranking fits a constant chat-template overhead per endpoint. "
                "verified=false entries are scored for display but never rendered as verdicts (PROJECT.md Q5).",
        "probes": PROBES,
        "entries": entries,
    }
    out = pathlib.Path(__file__).resolve().parent.parent / "pkg" / "fingerprint" / "db.json"
    out.write_text(json.dumps(db, ensure_ascii=False, indent=1) + "\n")
    for e in entries:
        print(e["id"], "verified" if e["verified"] else "UNVERIFIED", e["counts"])
    print("wrote", out)


if __name__ == "__main__":
    main()
