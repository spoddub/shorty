Build backend
FROM golang:1.25-alpine AS backend-builder
RUN apk add --no-cache git
WORKDIR /build/code

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
  go mod download

RUN go install github.com/pressly/goose/v3/cmd/goose@latest

COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /build/app .

# 3) Runtime
FROM alpine:3.22

WORKDIR /app

COPY --from=backend-builder /build/app /app/bin/app

COPY --from=backend-builder build/code/db/migrations /app/db/migrations
COPY --from=backend-builder /go/bin/goose /usr/local/bin/goose

COPY bin/run.sh /app/bin/run.sh
RUN chmod +x /app/bin/run.sh

EXPOSE 8080

CMD ["/app/bin/run.sh"]