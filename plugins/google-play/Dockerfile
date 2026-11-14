# gocdnext/google-play — publish a signed Android artifact (.aab/.apk) to the
# Google Play Developer API. Pairs with the `android` plugin: point it at the
# signed bundle the build produced.
#
# Contract (PLUGIN_* env, mapped from `with:`) — see entrypoint.sh. The
# service-account JSON (from secrets:) is written to a 0600 tempfile and passed
# to the Play API via GOOGLE_APPLICATION_CREDENTIALS; it never reaches argv.
#
# No JDK/Android SDK needed — this is a pure API client.
FROM python:3.12-slim-bookworm

# Pinned client libs (Renovate bumps the patch/minor; majors pinned). The Play
# publisher uses google-api-python-client (Androidpublisher v3) + google-auth
# (service-account credentials).
RUN pip install --no-cache-dir \
      google-api-python-client==2.149.0 \
      google-auth==2.35.0

RUN mkdir -p /opt/gplay
COPY publish.py /opt/gplay/publish.py
COPY entrypoint.sh /usr/local/bin/gocdnext-google-play
RUN chmod +x /usr/local/bin/gocdnext-google-play

WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/gocdnext-google-play"]
