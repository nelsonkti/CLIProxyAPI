#!/usr/bin/env python3
"""
Batch add fields to JSON files
Fields to add:
  quota_cooldown_threshold: 40
  imported_at: "2026-07-07T11:00:34Z"
"""

import json
import os
import sys
from pathlib import Path

def add_fields_to_json(file_path):
    try:
        with open(file_path, 'r', encoding='utf-8') as f:
            data = json.load(f)

        data['quota_cooldown_threshold'] = 40
        data['imported_at'] = '2026-07-07T11:00:34Z'

        with open(file_path, 'w', encoding='utf-8') as f:
            json.dump(data, f, indent=2, ensure_ascii=False)

        return True
    except Exception as e:
        print(f"Error: {file_path} - {e}")
        return False

def main():
    target_dir = sys.argv[1] if len(sys.argv) > 1 else '.'

    if not os.path.isdir(target_dir):
        print(f"Error: Directory '{target_dir}' does not exist")
        print(f"Usage: python3 {sys.argv[0]} [directory]")
        sys.exit(1)

    json_files = sorted(Path(target_dir).glob('*.json'))

    if not json_files:
        print(f"No .json files found in {target_dir}")
        sys.exit(0)

    count = 0
    failed = 0

    for file_path in json_files:
        if add_fields_to_json(file_path):
            count += 1
            print(f"✓ {file_path.name}")
        else:
            failed += 1

    print(f"\nDone. Updated: {count}, Failed: {failed}")

if __name__ == '__main__':
    main()
