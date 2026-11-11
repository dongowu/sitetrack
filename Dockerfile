# Dependencies are fetched at build time. go.mod and go.sum are copied on their
# own so the download layer is cached and only invalidated when a dependency
# actually changes, not on every source edit.
FROM golang:1.26-alpine AS build
WORKDIR /src

# Override with --build-arg GOPROXY=... if the default is slow from where you
# build. Kept as a build argument rather than a hardcoded mirror so the image is
# not pinned to one region's network.
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} GOSUMDB=sum.golang.org

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/seed ./cmd/seed

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 app

WORKDIR /app
COPY --from=build /out/api /out/seed /app/
USER app

EXPOSE 8080
ENTRYPOINT ["/app/api"]
