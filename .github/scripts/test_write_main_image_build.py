import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('build_evidence', Path(__file__).with_name('write-main-image-build.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class MainImageEvidenceTest(unittest.TestCase):
    def test_binds_digest_source_and_run(self):
        env = {'GITHUB_REPOSITORY': 'ai-workspace-services/billing-service', 'GITHUB_EVENT_NAME': 'push',
               'GITHUB_REF': 'refs/heads/main', 'GITHUB_SHA': 'a' * 40, 'CHECKED_OUT_SHA': 'a' * 40,
               'IMAGE_DIGEST': 'sha256:' + 'b' * 64, 'GITHUB_RUN_ID': '42', 'GITHUB_RUN_ATTEMPT': '1'}
        result = module.evidence(env)
        self.assertEqual(result['image'], 'ghcr.io/ai-workspace-services/billing-service@sha256:' + 'b' * 64)
        self.assertEqual((result['source_sha'], result['run_id'], result['attempt']), ('a' * 40, 42, 1))
        for key, bad in [('GITHUB_REPOSITORY', 'example/other'), ('GITHUB_EVENT_NAME', 'pull_request'),
                         ('GITHUB_REF', 'refs/heads/dev'), ('CHECKED_OUT_SHA', 'c' * 40),
                         ('IMAGE_DIGEST', 'latest'), ('GITHUB_RUN_ID', '0'), ('GITHUB_RUN_ATTEMPT', '0')]:
            with self.subTest(key=key), self.assertRaises(ValueError):
                module.evidence(dict(env, **{key: bad}))


if __name__ == '__main__':
    unittest.main()
