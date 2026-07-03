# syntax=docker/dockerfile:1

# ---- Build stage ----
# Pin the toolchain to the go.mod version so the build never falls back to an
# older Go that can't satisfy `go 1.26`.
FROM golang:1.26-alpine AS build
WORKDIR /src

# Download modules first so this layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

# Build a fully static binary (no CGO) so it runs on a minimal runtime image.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---- Runtime stage ----
FROM alpine:3.20
# ca-certificates: outbound TLS to managed Postgres. tzdata: Asia/Ho_Chi_Minh.
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/server /app/server

# The server binds to $PORT (falls back to 8080). PaaS platforms inject PORT.
EXPOSE 8080
ENTRYPOINT ["/app/server"]