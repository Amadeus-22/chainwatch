# syntax=docker/dockerfile:1

# --- web app ---
FROM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web ./
RUN npm run build

# --- build ---
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/chainwatch ./cmd/chainwatch

# --- run ---
# distroless/static: no shell, no package manager, CA certificates and tzdata included.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/chainwatch /chainwatch
COPY --from=web /web/dist /web
ENV CHAINWATCH_WEB_DIR=/web
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/chainwatch"]
