# PAY-1842 comparison repository

This intentionally broken service is the input to Gatemole's real-agent
before/after walkthrough. The existing tests pass, but they do not cover the
valid-refresh-token path or the access-token negative case. The human-owned
ticket in `tickets/PAY-1842.md` defines the required fix and test coverage.

Do not copy the implementation into a real authentication service.
