FROM scratch

COPY gatemole-production-fixture /gatemole-production-fixture

ENTRYPOINT ["/gatemole-production-fixture"]
