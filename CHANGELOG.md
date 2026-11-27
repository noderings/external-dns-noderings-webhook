# Changelog

All notable changes to this project are documented in this file. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/).

## [0.1.1](https://github.com/noderings/external-dns-noderings-webhook/compare/v0.1.0...v0.1.1) (2026-11-27)


### Documentation

* link the official Go SDK in the README ([#3](https://github.com/noderings/external-dns-noderings-webhook/issues/3)) ([86e8595](https://github.com/noderings/external-dns-noderings-webhook/commit/86e85950647e780f793acc95e89686242485db0c))

## [Unreleased]

## [0.1.0] - 2026-11-26

### Added

- Webhook provider for external-dns 0.24: reads and writes NodeRings DNS record sets for
  A, AAAA, CNAME, TXT, MX, SRV, NS, CAA, PTR and NAPTR endpoints, including the zone apex
  and nested zones.
- Domain filter and exclusions, default TTL, dry-run mode, token from an environment
  variable or a file, and a token check at start-up.
- Health (`/healthz`) and readiness (`/readyz`) endpoints for the official external-dns
  chart's probes.
- Values for the official external-dns Helm chart in `examples/values.yaml`.
- Multi-arch image `ghcr.io/noderings/external-dns-noderings-webhook` (linux/amd64, linux/arm64).
- Tests with external-dns's own planner and TXT registry, and an end-to-end test in kind
  against the NodeRings API.

[Unreleased]: https://github.com/noderings/external-dns-noderings-webhook/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/noderings/external-dns-noderings-webhook/releases/tag/v0.1.0
