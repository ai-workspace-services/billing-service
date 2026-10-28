#!/usr/bin/env python3
"""Bind a Main build's published GHCR digest to the checked-out source."""
import json
import os
from pathlib import Path
import re


def evidence(env):
    repo = env['GITHUB_REPOSITORY']
    sha = env['CHECKED_OUT_SHA']
    digest = env['IMAGE_DIGEST']
    run_id = env['GITHUB_RUN_ID']
    attempt = env['GITHUB_RUN_ATTEMPT']
    if (repo != 'ai-workspace-services/billing-service' or env['GITHUB_EVENT_NAME'] != 'push'
            or env['GITHUB_REF'] != 'refs/heads/main' or sha != env['GITHUB_SHA']
            or not re.fullmatch(r'[0-9a-f]{40}', sha)
            or not re.fullmatch(r'sha256:[0-9a-f]{64}', digest)
            or not re.fullmatch(r'[1-9][0-9]*', run_id)
            or not re.fullmatch(r'[1-9][0-9]*', attempt)):
        raise ValueError('invalid Main source or published image identity')
    return {'schema': 'main-image-build/v1', 'source_repository': repo, 'source_sha': sha,
            'image': f'ghcr.io/{repo}@{digest}', 'event': 'push', 'ref': 'refs/heads/main',
            'run_id': int(run_id), 'attempt': int(attempt)}


if __name__ == '__main__':
    Path(os.environ['EVIDENCE_PATH']).write_text(json.dumps(evidence(os.environ), sort_keys=True) + '\n')
