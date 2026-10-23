export function deviceView({ hasAuthorization, isSignedIn, isComplete }) {
  return {
    showCodeForm: !hasAuthorization && !isComplete,
    showRequest: hasAuthorization && !isComplete,
    showSignIn: hasAuthorization && !isSignedIn && !isComplete,
    showConsent: hasAuthorization && isSignedIn && !isComplete,
    showComplete: isComplete,
  };
}
