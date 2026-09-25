# Test image pipeline

Builds, signs, and pushes the image variants the gate integration tests
and the harness verify scenario check against. Not part of the swarmgate
binary — this is fixture infrastructure for exercising `internal/gate`.

```
./pipeline/build.sh --registry <host:port>
```

Requires `docker` and `cosign` on PATH. Idempotent: safe to re-run against
the same `--registry`; it reuses `pipeline/keys/` if already generated so
existing signatures stay verifiable.

## Variants

Each is pushed as `<registry>/swarmgate-test/<variant>:v1`.

| variant          | signed | attested | signed with |
|------------------|--------|----------|--------------|
| `ok-signed`      | yes    | no       | pipeline key |
| `ok-attested`    | yes    | yes (`slsaprovenance1`) | pipeline key |
| `unsigned`       | no     | no       | -            |
| `wrong-identity` | yes    | no       | throwaway key not in any policy |
| `no-attestation` | yes    | no       | pipeline key |

`unsigned` builds from distinct content (`hello-unsigned.txt`, not
`hello.txt`) so its digest is never accidentally identical to a signed
variant's — the other four share one digest since they're built from
identical content, which the harness verify scenario's `tag-repoint`
case relies on `unsigned` differing from to prove anything by retagging
onto it.

`pipeline/keys/` and `pipeline/signing-config.json` are generated on first
run and gitignored — they're fixture material, not secrets worth
versioning, and regenerating them doesn't change what the variants prove.

`ok-attested`'s SLSA v1 provenance (`pipeline/predicate.json`) names
`runDetails.builder.id` as `https://github.com/alexmchughdev/swarmgate/pipeline/build.sh`
— a gate policy checking `require_attestation` must set `builder_id` to
this exact string for `ok-attested` to pass; any other value rejects it
even though the signature and predicate type both still verify (see
`internal/gate/cosign_integration_test.go`'s
`TestIntegrationOkAttestedRejectsWrongBuilderID`).

## Verified behavior

Against a policy trusting `pipeline/keys/cosign.pub`:

```
$ cosign verify --key keys/cosign.pub --allow-http-registry --insecure-ignore-tlog \
    localhost:5000/swarmgate-test/ok-signed:v1
...
[{"critical":{"identity":{"docker-reference":"localhost:5000/swarmgate-test/ok-signed:v1"},
  "image":{"docker-manifest-digest":"sha256:db5d534663fc2a25aab4626a3ec8629de7126d420ca271fea3a3d0be21cd33d0"},
  "type":"https://sigstore.dev/cosign/sign/v1"},"optional":{}}]

$ cosign verify --key keys/cosign.pub --allow-http-registry --insecure-ignore-tlog \
    localhost:5000/swarmgate-test/unsigned:v1
Error: no signatures found

$ cosign verify --key keys/cosign.pub --allow-http-registry --insecure-ignore-tlog \
    localhost:5000/swarmgate-test/wrong-identity:v1
Error: no matching attestations: failed to verify signature: could not verify envelope:
accepted signatures do not match threshold, Found: 0, Expected 1

$ cosign verify-attestation --key keys/cosign.pub --type slsaprovenance1 \
    --allow-http-registry --insecure-ignore-tlog localhost:5000/swarmgate-test/ok-attested:v1
Verification for localhost:5000/swarmgate-test/ok-attested:v1 --
...
{"payload":"...","payloadType":"application/vnd.in-toto+json","signatures":[{"sig":"..."}]}

$ cosign verify-attestation --key keys/cosign.pub --type slsaprovenance1 \
    --allow-http-registry --insecure-ignore-tlog localhost:5000/swarmgate-test/no-attestation:v1
Error: none of the attestations matched the predicate type: slsaprovenance1,
found: https://sigstore.dev/cosign/sign/v1
```
