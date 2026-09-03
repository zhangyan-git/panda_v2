import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { App } from './App';

describe('App', () => {
  it('renders the platform skeleton content', () => {
    const markup = renderToStaticMarkup(<App />);

    expect(markup).toContain('Panda V2');
    expect(markup).toContain('Platform skeleton');
    expect(markup).toContain('业务页面和领域流程将在范围确认后实现。');
  });
});
