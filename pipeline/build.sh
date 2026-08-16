#!/usr/bin/env bash
# Builds, signs, and pushes the five swarmgate-test image variants consumed
# by the t6 gate harness scenarios. Idempotent: re-running rebuilds the same
# content and re-signs it against --registry.
#
# Variants:
#   ok-signed       signed with the pipeline key
#   ok-attested     signed with the pipeline key, carries a SLSA v1
#                   provenance attestation signed with the same key
#   unsigned        no signature, no attestation; distinct content from the
#                   other four (see hello-unsigned.txt) so its digest is
#                   never accidentally identical to a signed variant's
#   wrong-identity  signed with a throwaway key the policy does not trust
#   no-attestation  signed with the pipeline key, no attestation
set -euo pipefail

usage() {
  echo "usage: $0 --registry <host:port>" >&2
  exit 2
}

REGISTRY=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --registry) REGISTRY="$2"; shift 2 ;;
    *) usage ;;
  esac
done
[[ -n "$REGISTRY" ]] || usage

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KEY_DIR="$SCRIPT_DIR/keys"
mkdir -p "$KEY_DIR"

# Keys and the signing config are gitignored and regenerated on first run
# against a fresh checkout; re-running against an existing key directory
# reuses the same keys so old signatures stay verifiable.
if [[ ! -f "$KEY_DIR/cosign.key" ]]; then
  COSIGN_PASSWORD="" cosign generate-key-pair --output-key-prefix "$KEY_DIR/cosign"
fi
if [[ ! -f "$KEY_DIR/wrong.key" ]]; then
  COSIGN_PASSWORD="" cosign generate-key-pair --output-key-prefix "$KEY_DIR/wrong"
fi

# A home registry has no transparency log, so the signing config used for
# key-based signing here drops the default Rekor URLs rather than depending
# on network access to fetch one at build time.
SIGNING_CONFIG="$SCRIPT_DIR/signing-config.json"
if [[ ! -f "$SIGNING_CONFIG" ]]; then
  cosign signing-config create --no-default-rekor --out "$SIGNING_CONFIG"
fi

VARIANTS=(ok-signed ok-attested unsigned wrong-identity no-attestation)

for variant in "${VARIANTS[@]}"; do
  ref="$REGISTRY/swarmgate-test/$variant:v1"
  # --provenance/--sbom=false: buildx's default build attestations would
  # otherwise attach unrelated metadata to every variant, including the
  # ones meant to carry none.
  #
  # unsigned builds from distinct content (hello-unsigned.txt): the t6
  # tag-repoint harness scenario retags a live tag onto this variant's
  # digest specifically to prove it differs from ok-signed's, so the two
  # must not collide the way an identical trivial COPY would produce.
  content_file=hello.txt
  if [[ "$variant" == unsigned ]]; then
    content_file=hello-unsigned.txt
  fi
  docker build --provenance=false --sbom=false --build-arg "CONTENT_FILE=$content_file" \
    -t "$ref" -f "$SCRIPT_DIR/Dockerfile" "$SCRIPT_DIR"
  docker push "$ref"
done

sign() {
  local variant="$1" key="$2"
  COSIGN_PASSWORD="" cosign sign --yes --key "$key" --allow-http-registry \
    --signing-config "$SIGNING_CONFIG" "$REGISTRY/swarmgate-test/$variant:v1"
}

sign ok-signed     "$KEY_DIR/cosign.key"
sign ok-attested    "$KEY_DIR/cosign.key"
sign wrong-identity "$KEY_DIR/wrong.key"
sign no-attestation "$KEY_DIR/cosign.key"
# unsigned is left untouched.

COSIGN_PASSWORD="" cosign attest --yes --key "$KEY_DIR/cosign.key" \
  --predicate "$SCRIPT_DIR/predicate.json" --type slsaprovenance1 \
  --allow-http-registry --signing-config "$SIGNING_CONFIG" \
  "$REGISTRY/swarmgate-test/ok-attested:v1"

echo "pipeline: built, signed, and pushed all variants to $REGISTRY"
