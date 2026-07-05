"""Print the ffmpeg version used by this workspace."""

from __future__ import annotations

import shutil
import subprocess
import sys


def main() -> int:
    ffmpeg = shutil.which("ffmpeg")
    if ffmpeg is None:
        print("ffmpeg not found on PATH", file=sys.stderr)
        return 1
    result = subprocess.run([ffmpeg, "-version"], capture_output=True, check=False, text=True)
    if result.returncode != 0:
        print(result.stderr.strip() or "ffmpeg -version failed", file=sys.stderr)
        return result.returncode
    first_line = result.stdout.splitlines()[0] if result.stdout else "ffmpeg version unknown"
    print(first_line)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
