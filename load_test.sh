#!/bin/bash
for i in {1..15000}; do
  key="key$i"
  # Each value is ~10KB to exceed 100MB quickly
  value=$(head -c 10240 /dev/urandom | base64 | tr -d '\n')
  curl -s -X POST -H "Content-Type: application/json" \
    -d "{\"key\":\"$key\",\"value\":\"$value\"}" \
    http://localhost:8080/v1/set >/dev/null
  if (( i % 1000 == 0 )); then
    echo "Inserted $i keys..."
  fi
done
