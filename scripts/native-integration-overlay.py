#!/usr/bin/env python3
"""Expose the existing ALSA fake ONLY in an explicit Go test overlay.

The imported production package never includes _test.go. This overlay gives the
CLI integration tests that same fake without shipping a fake or test hook in the
binary. It does not modify sources or remove either hardware opener guard.
"""
import json
import pathlib
import sys

root = pathlib.Path(__file__).resolve().parents[1]
output = pathlib.Path(sys.argv[1]).resolve()
output.parent.mkdir(parents=True, exist_ok=True)
replace = {}
for source in sorted((root / "internal/alsapcm").glob("*_test.go")):
    if "package alsapcm\n" in source.read_text():
        target = source.with_name("zz_nativeintegration_" + source.stem + "support.go")
        replace[str(target)] = str(source)
if not replace:
    sys.exit("no package alsapcm test support found")
source = root / "cmd/bridge/output_dsdtest_integration_test.go"
enabled = output.with_suffix(".go.txt")
tag = "//go:build nativeintegration\n"
text = source.read_text()
if not text.startswith(tag) or text.count(tag) != 1:
    sys.exit("native integration test lost its exact leading build tag")
text = text.removeprefix(tag)
if "//go:build" in text or "// +build" in text:
    sys.exit("native integration test retains a build constraint")
enabled.write_text(text)
replace[str(source)] = str(enabled)
# The backend integration suite uses the same fake in a separate test package.
# Its accepting admission remains a _test.go function, never a binary option.
source = root / "internal/player/playback/native_integration_test.go"
if not source.exists():
    sys.exit("playback integration test missing")
text = source.read_text()
if not text.startswith(tag) or text.count(tag) != 1:
    sys.exit("playback integration test lost its exact leading build tag")
text = text.removeprefix(tag)
if "//go:build" in text or "// +build" in text:
    sys.exit("playback integration test retains a build constraint")
enabled = output.with_suffix(".playback.go.txt")
enabled.write_text(text)
replace[str(source)] = str(enabled)
output.write_text(json.dumps({"Replace": replace}, indent=2) + "\n")
