FROM scratch

COPY gatemole-model-broker /gatemole-model-broker

ENTRYPOINT ["/gatemole-model-broker"]
