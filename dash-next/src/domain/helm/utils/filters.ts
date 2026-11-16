import type { KubeObject } from '~/shared/types/kubernetes';

import { releaseKeyOf, revisionOf } from './selectors';

type LatestByRelease<TSecret> = Record<string, TSecret>;

const withLatest = <TSecret extends KubeObject>(
  latest: LatestByRelease<TSecret>,
  secret: TSecret,
): LatestByRelease<TSecret> => {
  const key = releaseKeyOf(secret);
  return Object.hasOwn(latest, key) && revisionOf(latest[key]) >= revisionOf(secret)
    ? latest
    : { ...latest, [key]: secret };
};

export const atLatestRevision = <TSecret extends KubeObject>(secrets: TSecret[]): TSecret[] =>
  Object.values(secrets.reduce<LatestByRelease<TSecret>>(withLatest, {}));
