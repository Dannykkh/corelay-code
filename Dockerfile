# Stage 1: Build frontend
FROM node:20-alpine AS frontend
WORKDIR /app/web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Stage 2: Build Go binary
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod ./
COPY . .
COPY --from=frontend /app/web/dist ./internal/server/webdist/
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /corelaycode ./cmd/proxy/
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /corelaycode-acp ./cmd/corelaycode-acp/
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /corelaycode-profile ./cmd/corelaycode-profile/

# Stage 3: Final image
FROM alpine:3.20
RUN apk add --no-cache ca-certificates git bash
COPY --from=builder /corelaycode /usr/local/bin/corelaycode
COPY --from=builder /corelaycode-acp /usr/local/bin/corelaycode-acp
COPY --from=builder /corelaycode-profile /usr/local/bin/corelaycode-profile
EXPOSE 4000
VOLUME /workspace
WORKDIR /workspace
ENTRYPOINT ["corelaycode"]
CMD ["-port", "4000"]
