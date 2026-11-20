#!/bin/sh
for f in /migrations/migrations/*_up.sql; do
  echo "Running $f"
  psql -U postgres -d postgres -f "$f"
done