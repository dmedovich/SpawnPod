# Build context: dist/VERSION after both architecture packages are built.
FROM scratch
ARG VERSION
ARG REVISION
ARG TARGETARCH
LABEL org.opencontainers.image.title="SpawnPod" \
      org.opencontainers.image.source="https://github.com/dmedovich/SpawnPod" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$REVISION
COPY spawnpod_${VERSION}_linux_${TARGETARCH}/spawnpod /spawnpod
COPY spawnpod_${VERSION}_linux_${TARGETARCH}/spawnpod-agent /spawnpod-agent
COPY spawnpod_${VERSION}_linux_${TARGETARCH}/spawnpod-source-proxy /spawnpod-source-proxy
USER 10001:10001
ENTRYPOINT ["/spawnpod"]
