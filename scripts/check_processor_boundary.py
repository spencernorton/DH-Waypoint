#!/usr/bin/env python3
"""Check active backend dependencies and Go syntax for embedded inference."""
import pathlib
import subprocess

if __name__ == "__main__":
    root = pathlib.Path(__file__).resolve().parents[1]
    subprocess.run(["go", "run", "./scripts/processorboundary"], cwd=root, check=True)
