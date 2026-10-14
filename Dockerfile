FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS backend
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
COPY --from=web /src/backend/cmd/mdns-gateway/web/app/ ./cmd/mdns-gateway/web/app/
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/mdns-gateway ./cmd/mdns-gateway

FROM openresty/openresty:alpine
COPY --from=backend /out/mdns-gateway /usr/local/bin/mdns-gateway
RUN mkdir -p /data && chmod 700 /data
VOLUME ["/data"]
EXPOSE 80 8080 5353/udp
HEALTHCHECK --interval=30s --timeout=3s --start-period=15s CMD wget -q -O - http://127.0.0.1:8080/health || exit 1
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/mdns-gateway"]
