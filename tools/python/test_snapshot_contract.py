"""Fast stdlib tests for the optional Python snapshot contract checker."""
from __future__ import annotations

import copy
import json
import pathlib
import subprocess
import sys
import unittest

from snapshot_contract import ContractError, validate_snapshot

ROOT = pathlib.Path(__file__).resolve().parents[2]
FIXTURE = ROOT / "testdata/snapshot/synthetic-complete.json"
EXPECTED_REPO = "example/ghx-test"


def sample():
    return json.loads(FIXTURE.read_bytes())


def encode(sample_data):
    return json.dumps(sample_data).encode()


def pr(data):
    return data["data"]["repository"]["pullRequests"]["nodes"][0]


class SnapshotContractTests(unittest.TestCase):
    def validate(self, data, repo=EXPECTED_REPO):
        return validate_snapshot(encode(data), repo)

    def blocked(self, data, code):
        with self.assertRaises(ContractError) as caught:
            self.validate(data)
        self.assertEqual(str(caught.exception), code)

    def test_fixture_is_consistent(self):
        verdict = self.validate(sample())
        self.assertEqual(verdict["decision"], "OBSERVED")
        self.assertEqual(verdict["pr_count"], 1)
        self.assertEqual(verdict["check_count"], 2)
        self.assertFalse(verdict["merge_authority"])
        self.assertFalse(verdict["cache_freshness_verified"])

    def test_real_graphql_data_wrapper_is_required(self):
        self.blocked(sample()["data"], "MISSING_DATA")

    def test_missing_data(self):
        self.blocked({"data": None}, "MISSING_DATA")

    def test_partial_graphql_errors(self):
        data = sample(); data["errors"] = [{"message": "partial failure"}]
        self.blocked(data, "GRAPHQL_ERRORS")

    def test_wrong_repo(self):
        with self.assertRaisesRegex(ContractError, "WRONG_REPOSITORY"):
            self.validate(sample(), "example/other")

    def test_incomplete_pr_page(self):
        data = sample(); data["data"]["repository"]["pullRequests"]["pageInfo"]["hasNextPage"] = True
        self.blocked(data, "PARTIAL_PR_LIST")

    def test_incomplete_file_page(self):
        data = sample(); pr(data)["files"]["pageInfo"]["hasNextPage"] = True
        self.blocked(data, "PARTIAL_FILE_LIST")

    def test_bad_file_record_is_rejected(self):
        data = sample(); pr(data)["files"]["nodes"][0]["additions"] = "1"
        self.blocked(data, "INVALID_SCHEMA")

    def test_bad_review_record_is_rejected(self):
        data = sample(); pr(data)["reviews"]["totalCount"] = 1
        pr(data)["reviews"]["nodes"] = [{"state": "APPROVED", "submittedAt": None, "author": {"login": 7}, "commit": None}]
        self.blocked(data, "INVALID_SCHEMA")

    def test_missing_file_row(self):
        data = sample(); pr(data)["files"]["nodes"] = []
        self.blocked(data, "PARTIAL_FILE_LIST")

    def test_incomplete_review_page(self):
        data = sample(); pr(data)["reviews"]["pageInfo"]["hasPreviousPage"] = True
        self.blocked(data, "PARTIAL_REVIEW_LIST")

    def test_missing_review_row(self):
        data = sample(); pr(data)["reviews"]["totalCount"] = 1
        self.blocked(data, "PARTIAL_REVIEW_LIST")

    def test_incomplete_check_contexts(self):
        data = sample(); pr(data)["commits"]["nodes"][0]["commit"]["statusCheckRollup"]["contexts"]["pageInfo"]["hasNextPage"] = True
        self.blocked(data, "PARTIAL_CHECK_LIST")

    def test_mismatched_head_commit(self):
        data = sample(); pr(data)["commits"]["nodes"][0]["commit"]["oid"] = "c" * 40
        self.blocked(data, "INVALID_SCHEMA")

    def test_coerced_draft_rejected(self):
        data = sample(); pr(data)["isDraft"] = "false"
        self.blocked(data, "INVALID_SCHEMA")

    def test_incomplete_check_producer_rejected(self):
        data = sample(); pr(data)["commits"]["nodes"][0]["commit"]["statusCheckRollup"]["contexts"]["nodes"][0]["checkSuite"] = None
        self.blocked(data, "INVALID_SCHEMA")

    def test_duplicate_pr(self):
        data = sample(); nodes=data["data"]["repository"]["pullRequests"]["nodes"]
        nodes.append(copy.deepcopy(nodes[0]))
        self.blocked(data, "DUPLICATE_PR")

    def test_duplicate_json_key(self):
        raw = b'{"data":{},"data":{}}'
        with self.assertRaisesRegex(ContractError, "DUPLICATE_JSON_KEY"):
            validate_snapshot(raw, EXPECTED_REPO)

    def test_nan_rejected(self):
        with self.assertRaisesRegex(ContractError, "INVALID_JSON_CONSTANT"):
            validate_snapshot(b'{"data":NaN}', EXPECTED_REPO)

    def test_invalid_payload_type(self):
        with self.assertRaisesRegex(ContractError, "INVALID_SIZE"):
            validate_snapshot("data", EXPECTED_REPO)

    def test_oversized_input(self):
        with self.assertRaisesRegex(ContractError, "INVALID_SIZE"):
            validate_snapshot(b" " * (8 * 1024 * 1024 + 1), EXPECTED_REPO)

    def test_null_check_rollup_is_valid_observationally(self):
        data = sample(); pr(data)["commits"]["nodes"][0]["commit"]["statusCheckRollup"] = None
        self.assertEqual(self.validate(data)["check_count"], 0)

    def test_empty_open_pr_page_is_complete(self):
        data = sample(); data["data"]["repository"]["pullRequests"]["nodes"] = []
        self.assertEqual(self.validate(data)["pr_count"], 0)

    def test_cli_pass_and_fail_codes(self):
        cmd = [sys.executable, str(ROOT / "tools/python/snapshot_contract.py"), "--repo", EXPECTED_REPO, str(FIXTURE)]
        ok = subprocess.run(cmd, capture_output=True, text=True, timeout=5)
        self.assertEqual(ok.returncode, 0, ok.stderr)
        self.assertEqual(json.loads(ok.stdout)["decision"], "OBSERVED")
        bad = subprocess.run(cmd[:-2] + ["example/other", str(FIXTURE)], capture_output=True, text=True, timeout=5)
        self.assertEqual(bad.returncode, 2, bad.stderr)
        self.assertEqual(json.loads(bad.stdout)["code"], "WRONG_REPOSITORY")

    def test_go_query_has_required_fields(self):
        # Protect the Python/Go contract from drift. This is a source assertion,
        # not a substitute for GitHub's live GraphQL API schema.
        source_path = ROOT / "src/cmd/ghx/snapshot.go"
        if not source_path.exists():
            self.skipTest("requires full ghx source checkout")
        src = source_path.read_text()
        query = src.split("const snapshotQuery = `", 1)[1].split("`", 1)[0]
        for field in ("nameWithOwner", "defaultBranchRef", "pageInfo { hasNextPage endCursor }",
                      "files(first: 100)", "reviews(last: 100)", "commits(last: 1)",
                      "contexts(first: 100)", "__typename", "databaseId", "checkSuite"):
            with self.subTest(field=field):
                self.assertIn(field, query)


if __name__ == "__main__":
    unittest.main()
