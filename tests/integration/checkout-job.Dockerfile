FROM docker.io/library/node@sha256:afdf98210b07b586eb71fa22ba2e432e058e4cd1304d31ed60888755b8c865fb
RUN apk add --no-cache git ca-certificates
COPY ca.crt /usr/local/share/ca-certificates/spawnpod-local.crt
RUN update-ca-certificates
