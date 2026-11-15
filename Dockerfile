# One image with every binary; each compose service picks its command (docs/f11.md).
# The Go version is the one mise.toml pins; mise run gen:check fails when they differ.
FROM golang:1.27.4 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ \
    ./cmd/api ./cmd/worker ./cmd/pixsim ./cmd/cardsim ./cmd/boletosim ./cmd/migrate ./cmd/admin

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
USER nonroot:nonroot
