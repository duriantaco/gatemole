FROM scratch

COPY vouch-production-fixture /vouch-production-fixture

ENTRYPOINT ["/vouch-production-fixture"]
