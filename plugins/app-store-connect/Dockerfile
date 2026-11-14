# gocdnext/app-store-connect — upload an already-built IPA from Linux through
# Apple's Build Upload API, then optionally distribute the processed build to
# TestFlight groups. Xcode is required only for the upstream build, not here.
FROM python:3.13-slim-bookworm

RUN pip install --no-cache-dir \
      PyJWT==2.15.1 \
      cryptography==50.0.2 \
      requests==2.34.2

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

RUN mkdir -p /opt/app-store-connect
COPY publish.py /opt/app-store-connect/publish.py
COPY entrypoint.sh /usr/local/bin/gocdnext-app-store-connect
RUN chmod +x /usr/local/bin/gocdnext-app-store-connect

WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/gocdnext-app-store-connect"]
