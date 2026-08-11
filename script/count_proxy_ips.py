#!/usr/bin/env python3
"""Count unique proxy IP addresses in JSON files."""

import argparse
import ipaddress
import json
import sys
from collections import Counter
from pathlib import Path
from urllib.parse import urlsplit


def extract_proxy_ip(proxy_url):
    if not isinstance(proxy_url, str) or not proxy_url.strip():
        return None

    value = proxy_url.strip()
    parsed = urlsplit(value if "://" in value else f"//{value}")

    try:
        host = parsed.hostname
    except ValueError:
        return None

    if not host:
        return None

    try:
        return str(ipaddress.ip_address(host))
    except ValueError:
        return None


def read_proxy_ip(file_path):
    try:
        with file_path.open("r", encoding="utf-8") as file:
            data = json.load(file)
    except (OSError, json.JSONDecodeError) as error:
        return None, f"{file_path}: {error}"

    if not isinstance(data, dict):
        return None, f"{file_path}: JSON root is not an object"

    proxy_url = data.get("proxy_url")
    proxy_ip = extract_proxy_ip(proxy_url)
    if proxy_ip:
        return proxy_ip, None

    if proxy_url:
        return None, f"{file_path}: proxy_url does not contain a valid IP address"

    return None, None


def parse_args():
    parser = argparse.ArgumentParser(
        description="Extract proxy IPs from proxy_url fields and count duplicates."
    )
    parser.add_argument(
        "directory",
        nargs="?",
        default=".",
        help="directory containing JSON files (default: current directory)",
    )
    parser.add_argument(
        "-r",
        "--recursive",
        action="store_true",
        help="scan JSON files in subdirectories recursively",
    )
    return parser.parse_args()


def main():
    args = parse_args()
    target_dir = Path(args.directory).expanduser()

    if not target_dir.is_dir():
        print(f"Error: Directory '{target_dir}' does not exist", file=sys.stderr)
        return 1

    pattern = "**/*.json" if args.recursive else "*.json"
    json_files = sorted(target_dir.glob(pattern))

    if not json_files:
        print(f"No .json files found in {target_dir}")
        return 0

    ip_counts = Counter()
    files_without_proxy_ip = 0
    errors = []

    for file_path in json_files:
        proxy_ip, error = read_proxy_ip(file_path)
        if error:
            errors.append(error)
        if proxy_ip:
            ip_counts[proxy_ip] += 1
        else:
            files_without_proxy_ip += 1

    print("Proxy IP statistics:")
    if ip_counts:
        for proxy_ip, count in sorted(
            ip_counts.items(), key=lambda item: (-item[1], item[0])
        ):
            print(f"{proxy_ip}: files={count}, duplicates={count - 1}")
    else:
        print("No valid proxy IP addresses found.")

    total_matches = sum(ip_counts.values())
    print("\nSummary:")
    print(f"JSON files scanned: {len(json_files)}")
    print(f"Files with proxy IP: {total_matches}")
    print(f"Unique proxy IPs: {len(ip_counts)}")
    print(f"Duplicate occurrences: {total_matches - len(ip_counts)}")
    print(f"Files without valid proxy IP: {files_without_proxy_ip}")

    if errors:
        print("\nWarnings:", file=sys.stderr)
        for error in errors:
            print(f"- {error}", file=sys.stderr)

    return 0


if __name__ == "__main__":
    sys.exit(main())
