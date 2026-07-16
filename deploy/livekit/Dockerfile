FROM livekit/livekit-server:v1.13.1
COPY deploy/livekit/livekit.yaml /etc/livekit.yaml
ENTRYPOINT ["/livekit-server"]
CMD ["--config", "/etc/livekit.yaml"]
