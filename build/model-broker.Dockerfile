FROM scratch

COPY vouch-model-broker /vouch-model-broker

ENTRYPOINT ["/vouch-model-broker"]
