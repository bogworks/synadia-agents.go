# Security Policy

## Supported versions

Security fixes go into the latest 0.x release. Earlier 0.x releases are not patched.

## Reporting a vulnerability

Please do not open a public issue for a security problem.

Report it privately with a GitHub security advisory on this repository: open the **Security** tab,
choose **Report a vulnerability**, and describe the problem, the affected version and, if you can, a
way to reproduce it.

You can expect an acknowledgement within a few working days. We will work with you on a fix and on
a coordinated disclosure, and we will credit you in the advisory if you wish.

## Scope

The SDK handles signing seeds, credentials and message payloads. It must never log them. A path by
which one of them reaches a log, an error message or the wire without being meant to is a
vulnerability. So is any way to forge or spoof the sender identity of SAP section 13, or to make the
SDK accept a message it should reject.
