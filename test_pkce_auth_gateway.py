import unittest
from unittest import mock

import pkce_auth_gateway as gateway


class PKCEAuthGatewaySecurityTests(unittest.TestCase):
    def setUp(self):
        with gateway.SESSION.lock:
            self.original_state = gateway.SESSION.state
            self.original_verifier = gateway.SESSION.verifier
            gateway.SESSION.state = "expected-state"
            gateway.SESSION.verifier = "test-verifier"

    def tearDown(self):
        with gateway.SESSION.lock:
            gateway.SESSION.state = self.original_state
            gateway.SESSION.verifier = self.original_verifier

    def test_default_host_is_loopback(self):
        self.assertEqual(gateway.DEFAULT_HOST, "127.0.0.1")

    @mock.patch.object(gateway, "exchange_code")
    def test_callback_missing_state_is_rejected_before_exchange(self, exchange_code):
        callback = gateway.REDIRECT_URI + "?code=test-code"

        with self.assertRaisesRegex(ValueError, "missing the required state"):
            gateway.complete_with_callback(callback)

        exchange_code.assert_not_called()

    @mock.patch.object(gateway, "exchange_code")
    def test_callback_with_mismatched_state_is_rejected_before_exchange(self, exchange_code):
        callback = gateway.REDIRECT_URI + "?code=test-code&state=wrong-state"

        with self.assertRaisesRegex(ValueError, "does not match"):
            gateway.complete_with_callback(callback)

        exchange_code.assert_not_called()

    @mock.patch.object(gateway, "persist_tokens", return_value={"email": "test@example.com"})
    @mock.patch.object(gateway, "exchange_code", return_value={"access_token": "test-token"})
    def test_matching_state_allows_exchange(self, exchange_code, persist_tokens):
        callback = gateway.REDIRECT_URI + "?code=test-code&state=expected-state"

        account = gateway.complete_with_callback(callback)

        exchange_code.assert_called_once_with("test-code", "test-verifier")
        persist_tokens.assert_called_once_with({"access_token": "test-token"})
        self.assertEqual(account, {"email": "test@example.com"})


if __name__ == "__main__":
    unittest.main()
