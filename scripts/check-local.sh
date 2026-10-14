#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
test -z "$(gofmt -l backend/cmd backend/internal)"
(cd backend && go mod tidy && go vet ./... && go test ./...)
(cd web && npm ci && npm run build)
echo 'Local source and frontend checks passed.'
echo 'For container acceptance, run: sh scripts/check-container.sh'
