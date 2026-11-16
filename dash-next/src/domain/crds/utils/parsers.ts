import type { PathStep } from '../types/crds';

const fieldPattern = /^(?:\\.|[^.[])+/;
const indexPattern = /^-?\d+$/;
const quotedPattern = /^['"](.*)['"]$/;
const filterPattern = /^\?\(@\.([\w.-]+)\s*(==|!=)\s*['"]?([^'")]*)['"]?\)$/;

const fromBracket = (body: string): PathStep | undefined => {
  const quoted = quotedPattern.exec(body);
  const filter = filterPattern.exec(body);
  return body === '*'
    ? { type: 'all' }
    : indexPattern.test(body)
      ? { type: 'index', index: Number(body) }
      : quoted
        ? { type: 'field', name: quoted[1] ?? '' }
        : filter
          ? {
              type: 'filter',
              path: (filter[1] ?? '').split('.'),
              isEqual: filter[2] === '==',
              value: filter[3] ?? '',
            }
          : undefined;
};

const withRest = (step: PathStep | undefined, rest: string): PathStep[] | undefined => {
  const following = step ? fromSteps(rest) : undefined;
  return step && following ? [step, ...following] : undefined;
};

const fromBracketed = (source: string): PathStep[] | undefined => {
  const end = source.indexOf(']');
  return end < 0
    ? undefined
    : withRest(fromBracket(source.slice(1, end).trim()), source.slice(end + 1));
};

const fromField = (source: string): PathStep[] | undefined => {
  const [field = ''] = fieldPattern.exec(source) ?? [];
  return withRest(
    { type: 'field', name: field.replace(/\\(.)/g, '$1') },
    source.slice(field.length),
  );
};

function fromSteps(source: string): PathStep[] | undefined {
  return source === ''
    ? []
    : source.startsWith('.')
      ? fromSteps(source.slice(1))
      : source.startsWith('[')
        ? fromBracketed(source)
        : fromField(source);
}

export const fromColumnPath = (path: string): PathStep[] | undefined =>
  fromSteps(
    path
      .trim()
      .replace(/^\{|\}$/g, '')
      .replace(/^\$/, ''),
  );
