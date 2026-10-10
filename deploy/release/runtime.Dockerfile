# Build context: the extracted architecture-specific release archive.
# Controller, agent and source proxy share the same release identity.
FROM scratch
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="SpawnPod" \
      org.opencontainers.image.source="https://github.com/dmedovich/SpawnPod" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$REVISION
COPY spawnpod /spawnpod
COPY spawnpod-agent /spawnpod-agent
COPY spawnpod-source-proxy /spawnpod-source-proxy
USER 10001:10001
ENTRYPOINT ["/spawnpod"]
