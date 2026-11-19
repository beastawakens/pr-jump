#!/bin/sh
# Waits for pr-jump on port $1 to index beastawakens, then checks that it sends
# "pr 1" to a real PR page and serves its OpenSearch description.
port=$1
for i in $(seq 1 120); do
  location=$(curl -s -o /dev/null -w '%{redirect_url}' -H 'Sec-Fetch-Site: none' "http://127.0.0.1:$port/?q=1") || true
  case "$location" in */pull/1) break ;; esac
  sleep 1
done
echo "location: $location"
case "$location" in */pull/1) ;; *) exit 1 ;; esac
curl -sf "http://127.0.0.1:$port/opensearch.xml" | grep -q "template=\"http://127.0.0.1:$port/?q={searchTerms}\""
