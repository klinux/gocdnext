# gocdnext/ruby — Ruby toolchain plugin for gocdnext pipelines.
#
# Contract:
#   PLUGIN_COMMAND      (required)  shell command after the install
#                                   step. Examples: "bundle exec rspec",
#                                   "bundle exec rubocop".
#   PLUGIN_WORKING_DIR  (optional)  path under /workspace.
#   PLUGIN_NO_INSTALL   (optional)  skip `bundle install`.
#
# build-base + headers ride along because the Ruby ecosystem leans
# hard on native extensions (nokogiri, pg, ffi) — an image that
# can't compile them fails most real Gemfiles on the first run.
# Same trade the go plugin makes shipping gcc for -race.
FROM ruby:3.3-alpine

RUN apk add --no-cache git build-base yaml-dev zlib-dev openssl-dev

COPY entrypoint.sh /usr/local/bin/gocdnext-ruby
RUN chmod +x /usr/local/bin/gocdnext-ruby

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-ruby"]
