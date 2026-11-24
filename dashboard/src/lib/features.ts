// Which optional parts of the API this agent serves (GET /v1/version
// `features`). A control whose feature is not on is hidden, not shown and
// left to fail. An agent that sends no feature list (older builds, or the
// version not loaded yet) is treated as serving none of them: those builds
// had none of these routes.
import type { FeatureKey, VersionInfo } from '../api/types';

export function hasFeature(version: VersionInfo | null | undefined, key: FeatureKey): boolean {
  return version?.features?.[key] === true;
}
