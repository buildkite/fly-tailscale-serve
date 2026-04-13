#!/usr/bin/env bash

set -euo pipefail

if [ -z "$BUILDKITE_TAG" ]; then
  echo "Releases only run when a new tag is pushed to github.com"
  exit 0
else
  echo "Preparing to release: ${BUILDKITE_TAG}"
fi

echo "--- Checking GitHub token"

if [ -z "$GITHUB_TOKEN" ]; then
  echo "\$GITHUB_TOKEN env variable must contain a Github API token with permission to create releases in buildkite/fly-tailscale-serve"
  exit 1
fi

echo "--- Ensuring tag exists locally"
git tag -f "$BUILDKITE_TAG" HEAD

echo "--- Running goreleaser"

export GORELEASER_CURRENT_TAG="$BUILDKITE_TAG"
export GORELEASER_PREVIOUS_TAG=$(git tag -l --sort=-version:refname | grep -v '-' | grep -v "^${BUILDKITE_TAG}$" | head -1)
echo "Current tag: $GORELEASER_CURRENT_TAG"
echo "Previous tag: $GORELEASER_PREVIOUS_TAG"

goreleaser release --clean
