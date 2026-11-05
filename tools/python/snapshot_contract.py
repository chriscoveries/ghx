#!/usr/bin/env python3
"""Offline, read-only Pydantic v2 contract check for `ghx snapshot` output.

Checks the current GraphQL `data.repository` wire format, not GitHub auth,
cache freshness or merge authority. This script does no network operations.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, ValidationError, model_validator

MAX_BYTES = 8 * 1024 * 1024
SHA = r"^[0-9a-fA-F]{40}$"


class ContractError(ValueError):
    """A stable, non-sensitive refusal reason."""


class GitHubModel(BaseModel):
    # GraphQL may add fields, but every field used by the contract is strict.
    model_config = ConfigDict(strict=True, extra="ignore")


class Page(GitHubModel):
    hasNextPage: bool | None = None
    hasPreviousPage: bool | None = None


class FileRecord(GitHubModel):
    path: str = Field(min_length=1)
    additions: int = Field(ge=0)
    deletions: int = Field(ge=0)


class ReviewUser(GitHubModel):
    login: str = Field(min_length=1)


class ReviewCommit(GitHubModel):
    oid: str = Field(pattern=SHA)


class ReviewRecord(GitHubModel):
    state: str
    submittedAt: str | None
    author: ReviewUser | None
    commit: ReviewCommit | None


class FileConnection(GitHubModel):
    pageInfo: Page
    totalCount: int = Field(ge=0)
    nodes: list[FileRecord] = Field(max_length=100)

    @model_validator(mode="after")
    def full_page(self):
        if self.pageInfo.hasNextPage is None:
            raise ValueError("missing file pagination flag")
        if len(self.nodes) > self.totalCount:
            raise ValueError("file count is inconsistent")
        return self


class ReviewsConnection(GitHubModel):
    pageInfo: Page
    totalCount: int = Field(ge=0)
    nodes: list[ReviewRecord] = Field(max_length=100)

    @model_validator(mode="after")
    def full_page(self):
        if self.pageInfo.hasPreviousPage is None:
            raise ValueError("missing review pagination flag")
        if len(self.nodes) > self.totalCount:
            raise ValueError("review count is inconsistent")
        return self


class Check(GitHubModel):
    kind: Literal["CheckRun", "StatusContext"] = Field(alias="__typename")
    databaseId: int | None = None
    name: str | None = None
    status: str | None = None
    conclusion: str | None = None
    context: str | None = None
    state: str | None = None
    checkSuite: dict | None = None

    @model_validator(mode="after")
    def fields_by_kind(self):
        if self.kind == "CheckRun":
            if not self.databaseId or not self.name or not self.status:
                raise ValueError("incomplete check run")
            if not self.checkSuite or not isinstance(self.checkSuite.get("app"), dict):
                raise ValueError("missing check producer")
            if not isinstance(self.checkSuite["app"].get("slug"), str):
                raise ValueError("missing check producer slug")
        elif not self.context or not self.state:
            raise ValueError("incomplete status context")
        return self


class Contexts(GitHubModel):
    pageInfo: Page
    nodes: list[Check] = Field(max_length=100)

    @model_validator(mode="after")
    def pagination_flag(self):
        if self.pageInfo.hasNextPage is None:
            raise ValueError("missing check pagination flag")
        return self


class Rollup(GitHubModel):
    contexts: Contexts


class Commit(GitHubModel):
    oid: str = Field(pattern=SHA)
    statusCheckRollup: Rollup | None = None


class CommitNode(GitHubModel):
    commit: Commit


class CommitConnection(GitHubModel):
    nodes: list[CommitNode] = Field(min_length=1, max_length=1)


class PR(GitHubModel):
    number: int = Field(gt=0)
    state: Literal["OPEN"]
    headRefOid: str = Field(pattern=SHA)
    isDraft: bool
    mergeStateStatus: str
    files: FileConnection
    reviews: ReviewsConnection
    commits: CommitConnection

    @model_validator(mode="after")
    def checked_head(self):
        if self.commits.nodes[0].commit.oid.lower() != self.headRefOid.lower():
            raise ValueError("commit identity does not match PR head")
        return self


class PRConnection(GitHubModel):
    pageInfo: Page
    nodes: list[PR] = Field(max_length=100)

    @model_validator(mode="after")
    def pagination_flag(self):
        if self.pageInfo.hasNextPage is None:
            raise ValueError("missing PR pagination flag")
        return self


class BranchTarget(GitHubModel):
    oid: str = Field(pattern=SHA)


class Branch(GitHubModel):
    name: str
    target: BranchTarget


class Repository(GitHubModel):
    nameWithOwner: str
    defaultBranchRef: Branch
    pullRequests: PRConnection


class Data(GitHubModel):
    repository: Repository


class Envelope(GitHubModel):
    data: Data | None = None
    errors: list[dict] | None = None


def _reject_duplicates(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ContractError("DUPLICATE_JSON_KEY")
        result[key] = value
    return result


def validate_snapshot(raw: bytes, expected_repo: str) -> dict:
    """Require a complete observed response, with no claim of trust/freshness."""
    if not isinstance(raw, bytes) or not raw or len(raw) > MAX_BYTES:
        raise ContractError("INVALID_SIZE")
    if expected_repo.count("/") != 1 or any(not x for x in expected_repo.split("/")):
        raise ContractError("INVALID_EXPECTED_REPO")
    try:
        data = json.loads(
            raw,
            object_pairs_hook=_reject_duplicates,
            parse_constant=lambda _: (_ for _ in ()).throw(ContractError("INVALID_JSON_CONSTANT")),
        )
        if not isinstance(data, dict):
            raise ContractError("INVALID_SCHEMA")
        envelope = Envelope.model_validate(data)
    except (ValueError, TypeError, UnicodeDecodeError, ValidationError) as exc:
        if isinstance(exc, ContractError):
            raise
        raise ContractError("INVALID_SCHEMA") from exc
    if envelope.errors:
        raise ContractError("GRAPHQL_ERRORS")
    if envelope.data is None:
        raise ContractError("MISSING_DATA")
    repo = envelope.data.repository
    if repo.nameWithOwner.casefold() != expected_repo.casefold():
        raise ContractError("WRONG_REPOSITORY")
    if repo.pullRequests.pageInfo.hasNextPage:
        raise ContractError("PARTIAL_PR_LIST")
    count_checks = 0
    seen = set()
    for pr in repo.pullRequests.nodes:
        if pr.number in seen:
            raise ContractError("DUPLICATE_PR")
        seen.add(pr.number)
        if pr.files.pageInfo.hasNextPage or len(pr.files.nodes) < pr.files.totalCount:
            raise ContractError("PARTIAL_FILE_LIST")
        if pr.reviews.pageInfo.hasPreviousPage or len(pr.reviews.nodes) < pr.reviews.totalCount:
            raise ContractError("PARTIAL_REVIEW_LIST")
        rollup = pr.commits.nodes[0].commit.statusCheckRollup
        if rollup is not None:
            if rollup.contexts.pageInfo.hasNextPage:
                raise ContractError("PARTIAL_CHECK_LIST")
            count_checks += len(rollup.contexts.nodes)
    return {
        "contract": "ghx-snapshot-graphql-v1",
        "decision": "OBSERVED",
        "repository": repo.nameWithOwner,
        "pr_count": len(seen),
        "check_count": count_checks,
        "complete": True,
        "merge_authority": False,
        "cache_freshness_verified": False,
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Validate ghx GraphQL snapshot bytes offline")
    parser.add_argument("--repo", required=True, help="Expected owner/repository")
    parser.add_argument("input", nargs="?", default="-", help="JSON file, or - for stdin")
    args = parser.parse_args(argv)
    try:
        if args.input == "-":
            raw = sys.stdin.buffer.read(MAX_BYTES + 1)
        else:
            with Path(args.input).open("rb") as src:
                raw = src.read(MAX_BYTES + 1)
        result = validate_snapshot(raw, args.repo)
    except OSError:
        result = {"decision": "BLOCK", "code": "INPUT_IO_ERROR", "complete": False}
    except ContractError as exc:
        result = {"decision": "BLOCK", "code": str(exc), "complete": False}
    print(json.dumps(result, sort_keys=True, separators=(",", ":")))
    return 0 if result["decision"] == "OBSERVED" else 2


if __name__ == "__main__":
    raise SystemExit(main())
