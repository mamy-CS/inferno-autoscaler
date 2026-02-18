#!/bin/sh
# Parallel Load Generator Script
#
# This script generates load by sending requests in parallel batches with sleep between batches.
# This is used by parallel load workers to generate concurrent load from multiple pods.
#
# Environment Variables:
#   WORKER_ID: Unique identifier for this worker (for logging)
#   TOTAL_REQUESTS: Total number of requests to send
#   BATCH_SIZE: Number of requests to send in parallel per batch
#   CURL_TIMEOUT: Timeout for each curl request (seconds)
#   MAX_TOKENS: Maximum tokens for each request
#   BATCH_SLEEP: Sleep duration between batches (seconds)
#   MODEL_ID: Model ID to use in requests
#   TARGET_URL: Target URL for requests (e.g., http://service:port/v1/completions)
#   MAX_RETRIES: Maximum number of retries for health check (default: 24)
#   RETRY_DELAY: Delay between retries (default: 5 seconds)

set -e

# Validate required environment variables
if [ -z "$WORKER_ID" ] || [ -z "$TOTAL_REQUESTS" ] || [ -z "$BATCH_SIZE" ] || [ -z "$TARGET_URL" ] || [ -z "$MODEL_ID" ]; then
  echo "ERROR: Missing required environment variables"
  echo "Required: WORKER_ID, TOTAL_REQUESTS, BATCH_SIZE, TARGET_URL, MODEL_ID"
  exit 1
fi

# Set defaults for optional variables
CURL_TIMEOUT=${CURL_TIMEOUT:-180}
MAX_TOKENS=${MAX_TOKENS:-400}
BATCH_SLEEP=${BATCH_SLEEP:-0.5}
MAX_RETRIES=${MAX_RETRIES:-24}
RETRY_DELAY=${RETRY_DELAY:-5}

# =============================================================================
# Script Start
# =============================================================================
echo "Load generator worker $WORKER_ID starting..."
echo "Sending $TOTAL_REQUESTS requests to $TARGET_URL"

# Wait for service to be ready
echo "Waiting for service to be ready..."
CONNECTED=false
for i in $(seq 1 $MAX_RETRIES); do
  if curl -s -o /dev/null -w "%{http_code}" "$TARGET_URL" 2>/dev/null | grep -qE "^(200|404)"; then
    echo "Connection test passed on attempt $i"
    CONNECTED=true
    break
  fi
  echo "Attempt $i failed, retrying in ${RETRY_DELAY}s..."
  sleep $RETRY_DELAY
done

if [ "$CONNECTED" != "true" ]; then
  echo "ERROR: Cannot connect to service after $MAX_RETRIES attempts"
  exit 1
fi

# Send requests aggressively in parallel batches (ignore individual curl failures)
SENT=0
while [ $SENT -lt $TOTAL_REQUESTS ]; do
  for i in $(seq 1 $BATCH_SIZE); do
    if [ $SENT -ge $TOTAL_REQUESTS ]; then break; fi
    (curl -s -o /dev/null --max-time $CURL_TIMEOUT -X POST "$TARGET_URL" \
      -H "Content-Type: application/json" \
      -d "{\"model\":\"$MODEL_ID\",\"prompt\":\"Write a detailed explanation of machine learning algorithms.\",\"max_tokens\":$MAX_TOKENS}" || true) &
    SENT=$((SENT + 1))
  done
  echo "Worker $WORKER_ID: sent $SENT / $TOTAL_REQUESTS requests..."
  sleep $BATCH_SLEEP
done

# Wait for all to complete at the end
wait || true

echo "Worker $WORKER_ID: completed all $TOTAL_REQUESTS requests"
exit 0
