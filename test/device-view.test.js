import { describe, expect, it } from "vitest";

import { deviceView } from "../public/device-view.js";

describe("OpenVaultDB device authorization wizard", () => {
  it("shows only code entry before a code is verified", () => {
    expect(
      deviceView({ hasAuthorization: false, isSignedIn: false, isComplete: false }),
    ).toEqual({
      showCodeForm: true,
      showRequest: false,
      showSignIn: false,
      showConsent: false,
      showComplete: false,
    });
  });

  it("replaces code entry with request details and sign-in after verification", () => {
    expect(
      deviceView({ hasAuthorization: true, isSignedIn: false, isComplete: false }),
    ).toEqual({
      showCodeForm: false,
      showRequest: true,
      showSignIn: true,
      showConsent: false,
      showComplete: false,
    });
  });

  it("replaces sign-in with consent for an authenticated user", () => {
    expect(
      deviceView({ hasAuthorization: true, isSignedIn: true, isComplete: false }),
    ).toEqual({
      showCodeForm: false,
      showRequest: true,
      showSignIn: false,
      showConsent: true,
      showComplete: false,
    });
  });

  it("shows only the terminal state after a decision", () => {
    expect(
      deviceView({ hasAuthorization: true, isSignedIn: true, isComplete: true }),
    ).toEqual({
      showCodeForm: false,
      showRequest: false,
      showSignIn: false,
      showConsent: false,
      showComplete: true,
    });
  });
});
