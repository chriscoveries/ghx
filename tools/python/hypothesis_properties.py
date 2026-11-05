"""Optional Hypothesis fuzz suite; run separately after installing exact dev pins.

No live GitHub calls. Hypothesis generates structural boundary errors that a
handwritten fixture may miss. This is not part of the Go runtime or CI today.
"""
import copy
import json
import unittest

from hypothesis import given, settings, strategies as st
from snapshot_contract import ContractError, validate_snapshot
from test_snapshot_contract import EXPECTED_REPO, sample


class SnapshotPropertyTests(unittest.TestCase):
    @settings(max_examples=80, deadline=None)
    @given(st.one_of(st.none(), st.booleans(), st.integers(), st.text(max_size=24)))
    def test_draft_field_accepts_only_real_bool(self, draft):
        data = sample()
        data["data"]["repository"]["pullRequests"]["nodes"][0]["isDraft"] = draft
        wire = json.dumps(data).encode()
        if type(draft) is bool:
            self.assertEqual(validate_snapshot(wire, EXPECTED_REPO)["decision"], "OBSERVED")
        else:
            with self.assertRaises(ContractError):
                validate_snapshot(wire, EXPECTED_REPO)

    @settings(max_examples=50, deadline=None)
    @given(st.lists(st.integers(min_value=1, max_value=100000), min_size=2, max_size=15))
    def test_duplicate_pr_numbers_always_block(self, numbers):
        data = sample()
        pr = data["data"]["repository"]["pullRequests"]["nodes"][0]
        pr["number"] = numbers[0]
        rows = [copy.deepcopy(pr)]
        for number in numbers[1:]:
            clone = copy.deepcopy(pr)
            clone["number"] = number
            rows.append(clone)
        rows[-1]["number"] = numbers[0]
        data["data"]["repository"]["pullRequests"]["nodes"] = rows
        with self.assertRaisesRegex(ContractError, "DUPLICATE_PR"):
            validate_snapshot(json.dumps(data).encode(), EXPECTED_REPO)

    @settings(max_examples=60, deadline=None)
    @given(st.sampled_from(["pullRequests", "files", "reviews", "contexts"]))
    def test_any_incomplete_page_blocks(self, dimension):
        data = sample()
        pr = data["data"]["repository"]["pullRequests"]["nodes"][0]
        if dimension == "pullRequests":
            data["data"]["repository"]["pullRequests"]["pageInfo"]["hasNextPage"] = True
        elif dimension == "files":
            pr["files"]["pageInfo"]["hasNextPage"] = True
        elif dimension == "reviews":
            pr["reviews"]["pageInfo"]["hasPreviousPage"] = True
        else:
            pr["commits"]["nodes"][0]["commit"]["statusCheckRollup"]["contexts"]["pageInfo"]["hasNextPage"] = True
        with self.assertRaisesRegex(ContractError, r"PARTIAL_"):
            validate_snapshot(json.dumps(data).encode(), EXPECTED_REPO)


if __name__ == "__main__":
    unittest.main()
