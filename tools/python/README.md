# Optional Python contract tooling for ghx snapshots

**Scope:** Developer/test-only. ghx and ghxd remain Go binaries. No Python is imported by the running cache or GARM runners. The script validates *syntactic completeness* of ghx's existing GraphQL `data.repository` response; it does not authenticate GitHub responses, certify cache freshness or authorize merges.

Uses Python 3.11+ and **Pydantic v2** for strict typed boundaries. An optional **Hypothesis** suite exercises mutated inputs and parser invariants. Both are open-source Python test dependencies. The production Go dependency graph remains unchanged.

Install in an isolated development environment (follow the repo's dependency policy for any CI promotion):

```sh
python3 -m venv .venv-ghx-contract
. .venv-ghx-contract/bin/activate
python -m pip install -r tools/python/requirements-dev.txt
python -m unittest discover -s tools/python -p 'test_*.py' -v
python -m unittest discover -s tools/python -p 'hypothesis_properties.py' -v
```

The second command works without Hypothesis installed; the third requires it. The code was first exercised with the locally available Pydantic 2.13.5; pinned 2.14.0 and optional Hypothesis must be run before making this a CI gate.

Validate a captured snapshot without any API calls:

```sh
python tools/python/snapshot_contract.py --repo example/ghx-test testdata/snapshot/synthetic-complete.json
# On a configured machine with ghx installed (network activity belongs to ghx):
ghx snapshot -R OWNER/REPO | python tools/python/snapshot_contract.py --repo OWNER/REPO -
```

It returns a single JSON verdict and exit code 0 for a **complete observational response**, or exit 2 with a stable reason on failure. It checks GraphQL errors, 100-item pagination limits for PRs/files/reviews/checks, wrong repository, head-commit identity, check producer fields and bad types. The synthetic fixture is invented test data, never user or legal content.

Current ghx `snapshot.go` only reads the first 100 open PRs, first 100 file paths, last 100 reviews and first 100 check contexts. The validator refuses if `pageInfo` indicates another page. It does not implement cursor retrieval. See [ghx #28](https://github.com/chriscoveries/ghx/issues/28) for a native versioned response with completeness and cache provenance. All consequential decisions still require fresh, authenticated verification through Devkit/Cifast.

**Adoption choice:** Keep Pydantic for the Python test/consumer contract. msgspec offers fast typed JSON decoding, but ghx already uses Go's decoder and is not bottlenecked by Python serialization. attrs/cattrs, dacite/mashumaro and Cerberus would duplicate the same model definitions here.
