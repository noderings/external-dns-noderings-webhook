# Security policy

## Reporting a vulnerability

Please do not open a public issue for security problems. Email **partners@noderings.com** with:

- a description of the issue and its impact
- steps to reproduce, or a proof of concept
- the image tag (`ghcr.io/noderings/external-dns-noderings-webhook:<tag>`) and external-dns version

We acknowledge reports within three working days and keep you updated until the issue is fixed. Credit is given in the release notes unless you ask us not to.

## Supported versions

Security fixes are made for the latest minor release.

## Handling credentials

The webhook reads the token from `NODERINGS_TOKEN` (or the file in `NODERINGS_TOKEN_FILE`), normally filled from a Kubernetes Secret. It never logs the token or request bodies. Give the service account only the DNS permissions it needs, and restrict who can read the Secret and exec into the external-dns pod.
