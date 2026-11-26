# Contributing

Thanks for helping improve external-dns-noderings-webhook.

1. Open an issue describing the bug or change before large pull requests.
2. Fork the repository and create a topic branch.
3. Run `make test` and `make lint`; both must pass. The unit tests use the in-memory
   NodeRings API in `internal/nrdns/nrdnstest`, so they need no token.
4. Add tests for new behaviour. For changes that reach the API, also run
   `NODERINGS_TOKEN=... hack/e2e.sh`; it creates and deletes a throwaway DNS zone.
5. Use [Conventional Commits](https://www.conventionalcommits.org/) and sign off each commit
   (`git commit -s`) to certify the [Developer Certificate of Origin](https://developercertificate.org/).

`internal/nrdns` is the NodeRings DNS client shared by
[external-dns-noderings-webhook](https://github.com/noderings/external-dns-noderings-webhook)
and [cert-manager-webhook-noderings](https://github.com/noderings/cert-manager-webhook-noderings).
Please describe fixes to it in an issue so they are made in both repositories.

Please report security problems privately as described in [SECURITY.md](SECURITY.md).
