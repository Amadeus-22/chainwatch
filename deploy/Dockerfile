# syntax=docker/dockerfile:1

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
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/chainwatch"]
