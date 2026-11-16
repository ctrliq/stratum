export const toRefusalText = (status: number, body: string): string =>
  body.trim() === '' ? `HTTP ${status}` : body.trim();
