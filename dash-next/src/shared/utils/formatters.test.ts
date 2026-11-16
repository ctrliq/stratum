import { describe, expect, it } from 'vitest';

import {
  toAge,
  toAgeAgo,
  toDisplayText,
  toErrorMessage,
  toErrorNotice,
  toJson,
  toLineCount,
  toListText,
  toLocalTime,
  toMebibytes,
  toMillicores,
  toPercentText,
  toSearchString,
  toStatusLabel,
  toStatusTone,
  toUsageTone,
  toYaml,
} from './formatters';

const now = Date.parse('2026-01-10T12:00:00Z');
const second = 1000;
const minute = 60 * second;
const hour = 60 * minute;
const day = 24 * hour;

const createdBefore = (elapsed: number): string => new Date(now - elapsed).toISOString();

describe('toAge', () => {
  it.each([
    [0, '0s'],
    [5 * second, '5s'],
    [89 * second, '89s'],
    [90 * second, '2m'],
    [12 * minute, '12m'],
    [89 * minute + 20 * second, '89m'],
    [90 * minute, '2h'],
    [3 * hour, '3h'],
    [47 * hour + 20 * minute, '47h'],
    [48 * hour, '2d'],
    [4 * day, '4d'],
    [10 * day, '10d'],
    [400 * day, '400d'],
  ])('shows %s ms elapsed as %s', (elapsed, age) => {
    expect(toAge(createdBefore(elapsed), now)).toBe(age);
  });

  it('has no age without a timestamp', () => {
    expect(toAge(undefined, now)).toBe('—');
  });

  it('reads a timestamp in the future as zero seconds', () => {
    expect(toAge(createdBefore(-30 * second), now)).toBe('0s');
  });

  it('has no age for a timestamp that is not a date', () => {
    expect(toAge('not a date', now)).toBe('—');
  });
});

describe('toDisplayText', () => {
  it('shows a dash for an absent or empty value and the value otherwise', () => {
    expect([undefined, null, '', 'cp-1', 0].map(toDisplayText)).toEqual([
      '—',
      '—',
      '—',
      'cp-1',
      '0',
    ]);
  });
});

describe('toMillicores and toMebibytes', () => {
  it.each([
    [250000000, 536870912, 250, 512],
    [1500000000, 1073741824, 1500, 1024],
    [0, 0, 0, 0],
  ])('converts %s nanocores and %s bytes', (nanocores, bytes, millicores, mebibytes) => {
    expect([toMillicores(nanocores), toMebibytes(bytes)]).toEqual([millicores, mebibytes]);
  });
});

describe('toJson', () => {
  it('indents by two spaces', () => {
    expect(toJson({ kind: 'Pod' })).toBe('{\n  "kind": "Pod"\n}');
  });
});

describe('toYaml', () => {
  it.each([
    ['nginx', 'value: nginx'],
    [null, 'value: null'],
    [3, 'value: 3'],
    [true, 'value: true'],
    ['true', 'value: "true"'],
    ['8080', 'value: "8080"'],
    ['a: b', 'value: "a: b"'],
    [' padded ', 'value: " padded "'],
    ['first\nsecond', 'value: "first\\nsecond"'],
    [[], 'value: []'],
    [{}, 'value: {}'],
  ])('writes %j so it reads back unchanged', (value, yaml) => {
    expect(toYaml({ value })).toBe(yaml);
  });

  it('writes an empty text as a single-quoted empty scalar', () => {
    expect(toYaml({ value: '' })).toBe("value: ''");
  });

  it('writes a list of maps as one dash item per map with its keys indented under', () => {
    expect(toYaml({ containers: [{ name: 'app', image: 'shop:1.4' }, { name: 'sidecar' }] })).toBe(
      'containers:\n  - name: app\n    image: shop:1.4\n  - name: sidecar',
    );
  });
});

describe('toYaml keys', () => {
  it('writes keys holding a slash, a space or a colon as they are', () => {
    expect(toYaml({ 'app.kubernetes.io/name': 'shop', 'two words': 1, 'f:metadata': 2 })).toBe(
      'app.kubernetes.io/name: shop\ntwo words: 1\nf:metadata: 2',
    );
  });

  it('writes an empty list or map that is the whole value in flow form', () => {
    expect([toYaml([]), toYaml({})]).toEqual(['[]', '{}']);
  });
});

describe('toLineCount', () => {
  it('counts lines, in the singular for one', () => {
    expect([toLineCount('a'), toLineCount('a\nb\nc')]).toEqual(['1 line', '3 lines']);
  });
});

describe('toErrorMessage', () => {
  it.each([
    [{ message: 'replicas must be >= 0' }, 'replicas must be >= 0'],
    [{ error: 'csrf' }, 'csrf'],
    ['upstream unavailable', 'upstream unavailable'],
    [undefined, 'HTTP 422'],
  ])('reads the refusal body %j as %s', (payload, message) => {
    expect(toErrorMessage({ status: 422, payload })).toBe(message);
  });

  it("reports the browser's message for a request that never reached the server", () => {
    expect(toErrorMessage({ status: 'unknown', payload: 'Failed to fetch' })).toBe(
      'Failed to fetch',
    );
  });

  it('reads the message of a thrown error', () => {
    expect(toErrorMessage(new Error('boom'))).toBe('boom');
  });
});

describe('toListText', () => {
  it('joins the values with a comma, or shows the absent mark for none', () => {
    expect([toListText(['Ingress', 'Egress']), toListText([])]).toEqual(['Ingress, Egress', '—']);
  });
});

describe('toLocalTime', () => {
  it("shows an instant in the browser's own format", () => {
    expect(toLocalTime('2026-11-13T12:00:00Z')).toBe(
      new Date('2026-11-13T12:00:00Z').toLocaleString(),
    );
  });

  it('shows an unparseable time as received and a missing one as the absent mark', () => {
    expect([toLocalTime('not-a-date'), toLocalTime(''), toLocalTime(undefined)]).toEqual([
      'not-a-date',
      '—',
      '—',
    ]);
  });
});

describe('toAgeAgo', () => {
  it.each([
    [0, '0s ago'],
    [59, '59s ago'],
    [59.6, '1m ago'],
    [60, '1m ago'],
    [119, '1m ago'],
    [3599, '59m ago'],
    [3600, '1h ago'],
    [86340, '23h ago'],
    [86400, '1d ago'],
    [864000, '10d ago'],
  ])('%s seconds reads %s', (elapsed, age) => {
    expect(toAgeAgo(createdBefore(elapsed * second), now)).toBe(age);
  });

  it('cannot be computed for a missing, unparseable or future time', () => {
    expect(
      [undefined, 'garbage', createdBefore(-30 * second)].map((timestamp) =>
        toAgeAgo(timestamp, now),
      ),
    ).toEqual([undefined, undefined, undefined]);
  });
});

describe('toErrorNotice', () => {
  it.each([
    [
      { status: 403, payload: { error: 'insufficient-role', message: 'admins only' } },
      'admins only',
    ],
    [{ status: 500, payload: 'boom' }, 'boom'],
    [{ status: 502, payload: '' }, 'HTTP 502'],
    [{ status: 'unknown', payload: 'Failed to fetch' }, 'Failed to fetch'],
    [new Error('The quota has been exceeded'), 'The quota has been exceeded'],
  ])('reports %o', (error, detail) => {
    expect(toErrorNotice(error)).toBe(`Error: ${detail}`);
  });
});

describe('toStatusLabel', () => {
  it('shows some statuses under a friendlier name and the rest unchanged', () => {
    expect(
      ['SchedulingDisabled', 'CrashLoopBackOff', 'healthy', 'Whatever'].map(toStatusLabel),
    ).toEqual(['Cordoned', 'CrashLoop', 'Healthy', 'Whatever']);
  });
});

describe('toStatusTone', () => {
  it('colours statuses by what they mean', () => {
    expect(
      ['Running', 'pending', 'Pending', 'Unschedulable', 'disconnected', 'Whatever'].map(
        toStatusTone,
      ),
    ).toEqual(['success', 'progress', 'neutral', 'warning', 'error', 'neutral']);
  });
});

describe('toUsageTone', () => {
  it('colours a meter by how full it is and has none without a value', () => {
    expect([undefined, 0, 1, 70, 71, 85, 86].map(toUsageTone)).toEqual([
      undefined,
      undefined,
      'success',
      'success',
      'warning',
      'warning',
      'error',
    ]);
  });
});

describe('toPercentText', () => {
  it('writes a share as a percentage', () => {
    expect(toPercentText(42)).toBe('42%');
  });
});

describe('toSearchString', () => {
  it('writes every value as plain text and repeats a list', () => {
    expect(toSearchString({ name: '123', ns: 'shop', op: ['a', 'b'], tab: undefined })).toBe(
      '?name=123&ns=shop&op=a&op=b',
    );
  });

  it('is empty without parameters', () => {
    expect(toSearchString({})).toBe('');
  });
});
