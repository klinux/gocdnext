#!/usr/bin/env python3
"""SMTP sender for gocdnext/email. Reads PLUGIN_* env, sends one
message, exits 0 on success / non-zero on failure with a short
stderr message."""

import os
import smtplib
import ssl
import sys
from email.message import EmailMessage


def split_addrs(raw: str) -> list[str]:
    return [a.strip() for a in raw.split(",") if a.strip()]


def main() -> int:
    host = os.environ["PLUGIN_HOST"]
    port = int(os.environ.get("PLUGIN_PORT") or "587")
    username = os.environ.get("PLUGIN_USERNAME") or ""
    password = os.environ.get("PLUGIN_PASSWORD") or ""
    tls_mode = (os.environ.get("PLUGIN_TLS") or "starttls").lower()
    sender = os.environ["PLUGIN_FROM"]
    to = split_addrs(os.environ["PLUGIN_TO"])
    cc = split_addrs(os.environ.get("PLUGIN_CC") or "")
    subject = os.environ["PLUGIN_SUBJECT"]
    body = os.environ["PLUGIN_BODY"]
    fmt = (os.environ.get("PLUGIN_FORMAT") or "plain").lower()

    if fmt not in ("plain", "html"):
        print(f"gocdnext/email: unknown format {fmt!r} (plain|html)", file=sys.stderr)
        return 2
    if tls_mode not in ("starttls", "tls", "none"):
        print(f"gocdnext/email: unknown tls mode {tls_mode!r}", file=sys.stderr)
        return 2

    msg = EmailMessage()
    msg["From"] = sender
    msg["To"] = ", ".join(to)
    if cc:
        msg["Cc"] = ", ".join(cc)
    msg["Subject"] = subject
    if fmt == "html":
        # Always include a plain fallback — Exchange / mobile mail
        # clients vary wildly in HTML handling, and a "click through
        # to see" experience is worse than a legible plain body.
        msg.set_content("(HTML email; view in an HTML-capable client.)")
        msg.add_alternative(body, subtype="html")
    else:
        msg.set_content(body)

    recipients = to + cc
    ctx = ssl.create_default_context()

    print(f"==> SMTP {host}:{port} ({tls_mode}) → {', '.join(recipients)}", flush=True)

    try:
        if tls_mode == "tls":
            server = smtplib.SMTP_SSL(host, port, context=ctx, timeout=30)
        else:
            server = smtplib.SMTP(host, port, timeout=30)
            if tls_mode == "starttls":
                server.starttls(context=ctx)
        with server:
            if username:
                server.login(username, password)
            server.send_message(msg, from_addr=sender, to_addrs=recipients)
    except smtplib.SMTPException as e:
        # stdlib puts credentials in str(e) for auth failures; the
        # runner masks secrets from logs already, but surface a
        # concise one-liner instead of a stack trace.
        print(f"gocdnext/email: smtp error: {e}", file=sys.stderr)
        return 1
    except OSError as e:
        print(f"gocdnext/email: network error: {e}", file=sys.stderr)
        return 1

    print("==> sent", flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
