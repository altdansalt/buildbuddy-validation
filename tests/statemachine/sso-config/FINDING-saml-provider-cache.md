# Cleared organization SSO remains available through cached SAML provider

Title: SetSSOConfig does not invalidate the SAML provider cache
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: An organization administrator clearing SSO sees it disabled in settings and public login discovery while SP metadata is still served. Replacement also retains the previous IdP login destination until cache expiry. Successful signed assertion acceptance is not established by this test.
Status: confirmed
Repro test targets: //tests/statemachine/sso-config:immediate_consistency_repro (clear); //tests/statemachine/sso-config:replacement_consistency_repro (replace)
BuildBuddy invocation: https://app.buildbuddy.io/invocation/acb84ea9-3557-4e2a-b25e-345e2a3103b1
Repeat invocation: https://app.buildbuddy.io/invocation/f15e37c3-12df-47dd-a08f-da965f68bf3f

Replacement reproduction: https://app.buildbuddy.io/invocation/cfbc0601-16a6-470a-a2c9-417f2bbbb5ed
Replacement repeat: https://app.buildbuddy.io/invocation/31271f0f-93bf-4295-8ea9-a0990a9b68cf
Both final repro targets: https://app.buildbuddy.io/invocation/31d4a805-bc92-41e6-98c9-dda06664d1c4

## Expected behavior

After clearing SetSSOConfig, the metadata endpoint should consult the cleared
organization state and redirect to the error page, matching an unconfigured
organization. Replacing the metadata URL should use the current IdP login target.

## Actual behavior

Set a valid fixture-local HTTPS metadata document, read SP metadata and start the
login flow to populate the provider cache, then clear via another organization
admin key. GetSSOConfig and GetGroup agree with the empty model on gRPC and HTTP,
including anonymous group discovery, but SP metadata remains HTTP 200 instead of
307. Login also remains HTTP 302 to the old IdP after clear. Two fresh remote
runs reproduce each isolated discrepancy above. Replacement returns the new URL
on database read surfaces but login still redirects to first.xml/login.

This is transient: the release uses a two-minute cache TTL. The prior local audit
also confirmed clear twice, and observed withdrawal at 125 seconds:
https://app.buildbuddy.io/invocation/77cac313-d012-4f63-93d0-80815eeb033e
https://app.buildbuddy.io/invocation/d3fee18f-5f74-4b89-a275-76c0bee9cfc7
Round-2 expanded coverage measured replacements at 117.012s and 117.017s,
and clears at 117.010s and 113.012s (one-second polling resolution), followed by
three stable observations over three seconds:
https://app.buildbuddy.io/invocation/ac2b45fc-b92e-43e1-a6c6-35a82e727791.
The cache TTL is measured from provider creation, not from the mutating RPC,
so repeated writes can shorten the remaining stale interval.

## Logs excerpt

```text
model="" metadata=200 login=302 destination=https://metadata.fixture/first.xml/login?...
model="https://127.0.0.1:44083/second.xml" metadata=200 login=302 destination=https://metadata.fixture/first.xml/login?...
immediate SAML consistency after step 1
Error: Should be true
```

All database read assertions execute before the failed assertion. The initial
unconfigured endpoint and configured XML/login endpoint expectations pass first.
Redirect following is disabled, so an error landing page cannot masquerade as
metadata XML. The provider is primed using a valid document and real TLS checks.

## Notes

Non-default prerequisites: auth.saml.cert_file and key_file enable SAML;
http.client.allow_localhost=true and SSL_CERT_FILE permit a trusted test-local
IdP document. These fixture options do not change provider cache behavior.
enterprise/server/saml/saml.go serviceProviderFromRequest returns its cached
provider before reading the group; SetSSOConfig updates SQLite without removing
that entry. No failures or malformed inputs are injected.

The default target accepts bounded convergence and checks three subsequent
observations over three seconds. The separate bug-tagged targets intentionally
requires immediate consistency and fails on this release. No upstream report or
PR was opened. Only this owned directory is changed.

Final coverage with login withdrawal checked after every clear also passed:
https://app.buildbuddy.io/invocation/545fe696-aa16-4782-8e01-43f7f1dfa443.
Replacement: 117.013s / 117.013s; clear: 117.011s / 113.011s;
all three subsequent stability reads passed.
