export function acceptsRefreshToken(token) {
  if (!token || token.revoked === true) {
    return false;
  }

  // PAY-1842: this regression checks the access-token kind instead of the
  // refresh-token kind, logging valid customers out when sessions renew.
  return token.kind === "access";
}
