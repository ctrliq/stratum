import type { Quantity } from '~/api/k8s/k8sSchemas';
import type { ObjectReference } from '~/shared/types/kubernetes';

export type JsonResult = { isValid: true; value: unknown } | { isValid: false; message: string };

const bytesPerMebibyte = 1024 ** 2;

const millicoresPerUnit: Record<string, number> = {
  '': 1000,
  m: 1,
  u: 1e-3,
  n: 1e-6,
};

const bytesPerUnit: Record<string, number> = {
  '': 1,
  m: 1e-3,
  k: 1e3,
  K: 1e3,
  M: 1e6,
  G: 1e9,
  T: 1e12,
  P: 1e15,
  E: 1e18,
  Ki: 1024,
  Mi: 1024 ** 2,
  Gi: 1024 ** 3,
  Ti: 1024 ** 4,
  Pi: 1024 ** 5,
  Ei: 1024 ** 6,
};

const quantityPattern = /^(\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)([A-Za-z]*)$/;

const fromQuantity = (quantity: Quantity | undefined, factors: Record<string, number>): number => {
  const [, amount = '0', unit = ''] = quantityPattern.exec(String(quantity ?? '')) ?? [];
  return Number(amount) * (Object.hasOwn(factors, unit) ? factors[unit] : 0);
};

export const fromCpuQuantity = (quantity: Quantity | undefined): number =>
  fromQuantity(quantity, millicoresPerUnit);

export const fromMemoryQuantity = (quantity: Quantity | undefined): number =>
  fromQuantity(quantity, bytesPerUnit) / bytesPerMebibyte;

export const fromObjectReference = (text: string): ObjectReference => {
  const [first = '', second] = text.split('/');
  return second === undefined ? { name: first } : { namespace: first, name: second };
};

export const fromJsonText = (text: string): JsonResult => {
  try {
    return { isValid: true, value: JSON.parse(text) };
  } catch (error) {
    return { isValid: false, message: error instanceof Error ? error.message : String(error) };
  }
};

const fromValues = (values: string[]): string | string[] =>
  values.length === 1 ? (values[0] ?? '') : values;

export const fromSearchString = (searchStr: string): Record<string, string | string[]> => {
  const params = new URLSearchParams(searchStr);
  return Object.fromEntries(
    [...new Set(params.keys())].map((key) => [key, fromValues(params.getAll(key))]),
  );
};
