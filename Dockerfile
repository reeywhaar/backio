# Download the prebuilt rclone binary and UPX-compress it.
# The stock binary is ~75 MB (all backends); UPX brings it to ~18 MB. This avoids
# compiling rclone's huge SDK dependency tree from source on every build.
FROM alpine:latest AS rclone-fetch
ARG RCLONE_VERSION=v1.74.3
ARG TARGETARCH=amd64
RUN apk add --no-cache curl unzip upx \
 && curl -fsSL -o /tmp/rclone.zip \
      "https://downloads.rclone.org/${RCLONE_VERSION}/rclone-${RCLONE_VERSION}-linux-${TARGETARCH}.zip" \
 && unzip -j /tmp/rclone.zip '*/rclone' -d /tmp \
 && upx --best --lzma /tmp/rclone

FROM golang:alpine AS builder
WORKDIR /app
COPY go.mod .
COPY main.go .
COPY internal/ internal/
RUN go build -trimpath -ldflags="-s -w" -o backio .

FROM alpine:latest
COPY --from=rclone-fetch /tmp/rclone /usr/bin/rclone
COPY --from=builder /app/backio /backio
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh /backio /usr/bin/rclone
ENV PORT=8080
ENTRYPOINT ["/entrypoint.sh"]
