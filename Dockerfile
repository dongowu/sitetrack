# Dependencies are vendored, so the build needs no module proxy and produces the
# same binary on any machine.
FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
COPY vendor ./vendor
COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=linux go build -mod=vendor -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
 && CGO_ENABLED=0 GOOS=linux go build -mod=vendor -trimpath -ldflags="-s -w" -o /out/seed ./cmd/seed

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 app

WORKDIR /app
COPY --from=build /out/api /out/seed /app/
USER app

EXPOSE 8080
ENTRYPOINT ["/app/api"]
