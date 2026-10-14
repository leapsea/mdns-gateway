.PHONY: build test frontend docker

frontend:
	cd web && npm ci && npm run build

build: frontend
	cd backend && go build -o bin/mdns-gateway ./cmd/mdns-gateway

test:
	cd backend && go test ./internal/...

docker:
	sh build/build-local-image.sh
