import assert from "node:assert/strict";
import test from "node:test";

import { acceptsRefreshToken } from "../../src/auth/refresh-token.js";

test("rejects a missing token", () => {
  assert.equal(acceptsRefreshToken(null), false);
});

test("rejects a revoked refresh token", () => {
  assert.equal(
    acceptsRefreshToken({ kind: "refresh", revoked: true }),
    false,
  );
});
