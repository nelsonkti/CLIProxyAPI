#!/bin/bash

# Batch add fields to JSON files
# Fields to add:
#   quota_cooldown_threshold: 40
#   imported_at: "2026-07-07T11:00:34Z"

TARGET_DIR="${1:-.}"

if [ ! -d "$TARGET_DIR" ]; then
    echo "Error: Directory '$TARGET_DIR' does not exist"
    echo "Usage: $0 [directory]"
    exit 1
fi

count=0
failed=0

for file in "$TARGET_DIR"/*.json; do
    [ -e "$file" ] || { echo "No .json files found in $TARGET_DIR"; exit 0; }

    if jq '. + {quota_cooldown_threshold: 40, imported_at: "2026-07-07T11:00:34Z"}' "$file" > "$file.tmp" && mv "$file.tmp" "$file"; then
        ((count++))
    else
        rm -f "$file.tmp"
        echo "Failed: $file"
        ((failed++))
    fi
done

echo "Done. Updated: $count, Failed: $failed"
