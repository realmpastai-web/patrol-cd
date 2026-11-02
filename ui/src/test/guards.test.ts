import { fireEvent } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { beginTest, finishTest, takeTestFindings } from './guards';

describe('test guards', () => {
  it('records console.error output', () => {
    console.error('a finding');
    expect(takeTestFindings()).toEqual([{ kind: 'console.error', detail: 'a finding' }]);
  });

  // The cases below reach the jsdom instance's virtual console and never go through the console the
  // tests see, so a major of vitest that binds that console differently loses them without any test
  // failing.
  it('records console.error from a script jsdom runs', () => {
    const script = document.createElement('script');
    script.textContent = "console.error('from a script')";
    document.body.append(script);
    script.remove();
    expect(takeTestFindings()).toEqual([{ kind: 'console.error', detail: 'from a script' }]);
  });

  // While the page has an 'error' listener, as src/extensions/loader.ts does when a bundle loads,
  // vitest does not pass the exception on to the process and only jsdom reports it.
  it('records an exception jsdom reports', () => {
    const pageListener = () => undefined;
    window.addEventListener('error', pageListener);
    const button = document.createElement('button');
    button.addEventListener('click', () => {
      throw new Error('from a listener');
    });
    document.body.append(button);
    fireEvent.click(button);
    button.remove();
    window.removeEventListener('error', pageListener);
    expect(takeTestFindings()).toEqual([
      { kind: 'jsdom error', detail: expect.stringContaining('Uncaught [Error: from a listener]') as string },
    ]);
  });

  function submitAForm(): void {
    const form = document.createElement('form');
    const button = document.createElement('button');
    form.append(button);
    document.body.append(form);
    fireEvent.click(button);
    form.remove();
  }

  it.each([
    ['window.scrollTo', () => window.scrollTo(0, 0)],
    ['navigation', () => window.location.assign('https://example.test/')],
    ['window.open', () => void window.open('https://example.test/')],
    ['HTMLFormElement.prototype.requestSubmit', submitAForm],
  ])('records what jsdom does not implement: %s', (feature, act) => {
    act();
    expect(takeTestFindings()).toEqual([
      { kind: 'jsdom error', detail: expect.stringContaining(`Not implemented: ${feature}`) as string },
    ]);
  });

  it('fails the test with what it recorded', () => {
    window.scrollTo(0, 0);
    expect(finishTest).toThrow(/jsdom error: Error: Not implemented: window\.scrollTo/);
    beginTest();
  });
});
