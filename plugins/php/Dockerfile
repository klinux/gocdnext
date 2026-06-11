# gocdnext/php — PHP toolchain plugin for gocdnext pipelines.
#
# Contract:
#   PLUGIN_COMMAND      (required)  shell command after the install
#                                   step. Examples: "vendor/bin/phpunit",
#                                   "vendor/bin/phpstan analyse".
#   PLUGIN_WORKING_DIR  (optional)  path under /workspace.
#   PLUGIN_NO_INSTALL   (optional)  skip `composer install` (reuse a
#                                   vendor/ shipped via artifacts).
#
# Official CLI image + composer copied from its official image
# (the composer project's recommended multi-stage pattern). Single
# PHP line per plugin tag — the v1 contract tracks the current
# supported PHP; pin a digest in `uses:` if you need to freeze.
FROM php:8.3-cli-alpine

COPY --from=composer:2 /usr/bin/composer /usr/local/bin/composer
RUN apk add --no-cache git unzip

COPY entrypoint.sh /usr/local/bin/gocdnext-php
RUN chmod +x /usr/local/bin/gocdnext-php

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-php"]
