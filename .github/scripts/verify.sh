#!/usr/bin/env bash
# Edge's checks, run from the root of the parent repository with Edge's submodule at the commit under
# test: the Nx targets, and the scan of the image between image and scenario-test (Edge ADR 0002).
set -euo pipefail

# Trivy, pinned by digest like every image that a check runs.
TRIVY=aquasec/trivy:0.68.1@sha256:a93fd67162843c0f749002af9245fe9a2e5edc41445bd71d3949c803e95ef05b
scan_dir=$(mktemp -d)
trap 'rm -rf "$scan_dir"' EXIT

# scan fails on a HIGH or CRITICAL finding that has a fix. The findings without one, which the test
# baseline carries until a release of APISIX moves it, are listed in the job summary.
scan() {
  docker save "$1" --output "$scan_dir/image.tar"
  local trivy=(docker run --rm --user "$(id -u):$(id -g)" --volume "$scan_dir:/scan" "$TRIVY" image
    --cache-dir /scan/cache --input /scan/image.tar --scanners vuln --severity HIGH,CRITICAL --quiet)
  "${trivy[@]}" --format table --output /scan/findings.txt
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    { echo "### HIGH and CRITICAL findings in $1"; echo '```'; cat "$scan_dir/findings.txt"; echo '```'; } >> "$GITHUB_STEP_SUMMARY"
  fi
  "${trivy[@]}" --ignore-unfixed --exit-code 1
}

export NX_NO_CLOUD=true
npm ci
npx nx run edge:lint
npx nx run edge:test
npx nx run edge:build
npx nx run edge:image
scan contelys/edge
npx nx run edge:scenario-test
