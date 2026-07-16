FROM scratch

LABEL io.palai.spike="runner-supervisor"

COPY palai-spike-engine /engine

USER 65532:65532
ENTRYPOINT ["/engine"]
