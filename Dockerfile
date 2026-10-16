FROM alpine:3.22

ARG TARGETARCH

COPY dist/linux-${TARGETARCH}/ws-downloader dist/linux-${TARGETARCH}/cli /usr/local/bin/
COPY config.example.yaml /etc/ws-file-share/config.example.yaml
RUN sed -i 's/^listen:.*/listen: "0.0.0.0:8080"/' /etc/ws-file-share/config.example.yaml
COPY --chmod=755 docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

WORKDIR /config

EXPOSE 8080
STOPSIGNAL SIGINT

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["serve"]
