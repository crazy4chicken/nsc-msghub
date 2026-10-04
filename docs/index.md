---
layout: home

hero:
  name: nsc-msghub
  text: Notification gateway microservice
  tagline: Submit one notification intent; the gateway resolves recipients, routes ordered channels, delivers, and records the result.
  actions:
    - theme: brand
      text: Get Started
      link: /guide/getting-started
    - theme: alt
      text: API Reference
      link: /api/overview

features:
  - title: One POST per notification
    details: A caller submits a user id, a type, and a body to POST /api/v1/notify, and never touches SMTP, phone numbers, or channel details.
  - title: Directory-resolved recipients
    details: Resolve addresses by user id from a reloadable local JSON table or an HTTP user service with {id} path placeholders, optional bearer auth, and explicit 404/502 semantics.
  - title: Ordered channel routing
    details: Map each notification type to a channel priority such as alert=email,sms;digest=email;default=email. An explicit channel bypasses routing, and unroutable notifications fail with a reason instead of silently going elsewhere.
  - title: Email and SMS channels
    details: Email over SMTP with implicit TLS, STARTTLS, or plaintext plus PLAIN/LOGIN auth and per-session timeouts; SMS through a provider interface that reports unavailability instead of faking a send.
  - title: Markdown-first bodies
    details: Choose bodyFormat text to only HTML-escape, or markdown to render per channel — inline-styled HTML with a plain-text fallback for email, de-syntaxed text for SMS. Bare HTML is rejected.
  - title: JSONL delivery records
    details: Every attempt is appended to a JSONL log with the chosen channel, status, and failure reason, queryable by channel, type, status, and user id.
  - title: Token or teamusers JWT auth
    details: Protect /api/* with a static bearer token, or hand authentication to teamusers for JWT plus permission checks with 401/403 semantics. /healthz stays public.
  - title: Zero third-party runtime dependencies
    details: Ships as a single self-contained binary; the official teamusers SDK is only involved when IAM authentication is enabled.
---
