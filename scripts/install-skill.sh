#!/bin/sh
# Install the xlfp agent skill (skills/xlfp/SKILL.md) into AI agent
# harnesses that use the shared SKILL.md format.
#
# Usage:
#   scripts/install-skill.sh [--target hermes|codex|Muse|muse|all]
#                            [--dest DIR] [--dry-run] [--force]
#
# With no --target, every detected harness (its home directory exists)
# gets the skill. --dest DIR installs under DIR/xlfp instead (useful
# for testing or other SKILL.md-compatible tools). The `muse` target
# only prints the `muse skills install` command, because that store
# owns its writes. Safe to re-run: an identical installed skill is
# left alone; a differing one needs --force.
set -eu

REPO="tyk-swe/xlfp"
RAW_URL="https://raw.githubusercontent.com/$REPO/main/skills/xlfp/SKILL.md"
TARGET=""
DEST=""
DRY_RUN=0
FORCE=0

say() { printf '%s\n' "$*"; }
die() { printf 'install-skill: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --target) TARGET="${2:-}"; shift 2 ;;
    --dest) DEST="${2:-}"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    --force) FORCE=1; shift ;;
    -h|--help) sed -n '2,16p' "$0" 2>/dev/null || say "usage: install-skill.sh [--target hermes|codex|Muse|muse|all] [--dest DIR] [--dry-run] [--force]"; exit 0 ;;
    *) die "unknown argument: $1 (try --help)" ;;
  esac
done

# --- Locate the skill source: this checkout, else download. ---
SRC_DIR=""
SCRIPT_DIR="$(cd "$(dirname "$0")" 2>/dev/null && pwd || true)"
if [ -n "$SCRIPT_DIR" ] && [ -f "$SCRIPT_DIR/../skills/xlfp/SKILL.md" ]; then
  SRC_DIR="$(cd "$SCRIPT_DIR/../skills/xlfp" && pwd)"
else
  TMP="$(mktemp -d)"
  trap 'rm -rf "$TMP"' EXIT INT TERM
  mkdir -p "$TMP/xlfp"
  if curl -fsSL -o "$TMP/xlfp/SKILL.md" "$RAW_URL"; then
    SRC_DIR="$TMP/xlfp"
  elif command -v gh >/dev/null 2>&1 && gh api -H "Accept: application/vnd.github.raw" "repos/$REPO/contents/skills/xlfp/SKILL.md" >"$TMP/xlfp/SKILL.md" && [ -s "$TMP/xlfp/SKILL.md" ]; then
    SRC_DIR="$TMP/xlfp"
  else
    die "could not download SKILL.md. If the repository is still private, clone it and run scripts/install-skill.sh from the clone."
  fi
fi
[ -f "$SRC_DIR/SKILL.md" ] || die "SKILL.md not found in $SRC_DIR"

installed_any=0
refused_any=0

install_into() { # $1 = label, $2 = skills root
  label="$1"; root="$2"; dest="$root/xlfp"
  if [ -f "$dest/SKILL.md" ]; then
    if cmp -s "$SRC_DIR/SKILL.md" "$dest/SKILL.md"; then
      say "[$label] already up to date: $dest"
      installed_any=1
      return 0
    fi
    if [ "$FORCE" -ne 1 ]; then
      say "[$label] SKIP: $dest/SKILL.md exists and differs (re-run with --force to replace)"
      refused_any=1
      return 0
    fi
  fi
  if [ "$DRY_RUN" -eq 1 ]; then
    say "[$label] dry-run: would install $SRC_DIR/SKILL.md -> $dest/SKILL.md"
    installed_any=1
    return 0
  fi
  mkdir -p "$dest"
  cp "$SRC_DIR/SKILL.md" "$dest/SKILL.md"
  say "[$label] installed: $dest/SKILL.md"
  installed_any=1
}

print_muse_instructions() {
  say "[muse] Muse Code manages its own skill store. Run this yourself:"
  say "  muse skills install $SRC_DIR"
  installed_any=1
}

if [ -n "$DEST" ]; then
  install_into "dest" "$DEST"
else
  case ",$TARGET," in
    *",hermes,"*) install_into "hermes" "$HOME/.hermes/skills" ;;
  esac
  case ",$TARGET," in
    *",codex,"*) install_into "codex" "$HOME/.codex/skills" ;;
  esac
  case ",$TARGET," in
    *",Muse,"*) install_into "Muse" "$HOME/.muse/skills" ;;
  esac
  case ",$TARGET," in
    *",muse,"*) print_muse_instructions ;;
  esac
  if [ -z "$TARGET" ] || [ "$TARGET" = "all" ]; then
    detected=0
    [ -d "$HOME/.hermes" ] && { install_into "hermes" "$HOME/.hermes/skills"; detected=1; }
    [ -d "$HOME/.codex" ] && { install_into "codex" "$HOME/.codex/skills"; detected=1; }
    [ -d "$HOME/.Muse" ] && { install_into "Muse" "$HOME/.muse/skills"; detected=1; }
    command -v muse >/dev/null 2>&1 && { print_muse_instructions; detected=1; }
    [ "$detected" -eq 1 ] || die "no supported agent harness detected (~/.hermes, ~/.codex, ~/.Muse, or the muse CLI). Use --target <name> to create one, or --dest DIR for another SKILL.md tool."
  elif [ "$installed_any" -eq 0 ] && [ "$refused_any" -eq 0 ]; then
    die "unknown --target: $TARGET (want hermes, codex, Muse, muse, or all)"
  fi
fi

[ "$installed_any" -eq 1 ] || die "nothing was installed"
say "Done. Restart your agent (or its session) so it picks up the xlfp skill."
