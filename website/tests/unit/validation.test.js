import { describe, it, expect } from 'vitest';
import {
  validatePageMetadata,
  validateNavItem,
  validateFeatureEntry,
  validatePricingPlans,
  validateBlogPost,
} from '../../src/js/validation.js';

describe('validatePageMetadata', () => {
  it('accepts a title between 10 and 70 characters', () => {
    expect(validatePageMetadata({ title: 'Valid Title Here' })).toEqual([]);
  });

  it('rejects a title shorter than 10 characters', () => {
    const errors = validatePageMetadata({ title: 'Short' });
    expect(errors.length).toBe(1);
    expect(errors[0]).toContain('10-70 characters');
  });

  it('rejects a title longer than 70 characters', () => {
    const errors = validatePageMetadata({ title: 'A'.repeat(71) });
    expect(errors.length).toBe(1);
    expect(errors[0]).toContain('10-70 characters');
  });

  it('accepts a title exactly 10 characters', () => {
    expect(validatePageMetadata({ title: 'A'.repeat(10) })).toEqual([]);
  });

  it('accepts a title exactly 70 characters', () => {
    expect(validatePageMetadata({ title: 'A'.repeat(70) })).toEqual([]);
  });

  it('accepts a description between 50 and 160 characters', () => {
    expect(validatePageMetadata({ description: 'A'.repeat(80) })).toEqual([]);
  });

  it('rejects a description shorter than 50 characters', () => {
    const errors = validatePageMetadata({ description: 'Too short' });
    expect(errors.length).toBe(1);
    expect(errors[0]).toContain('50-160 characters');
  });

  it('rejects a description longer than 160 characters', () => {
    const errors = validatePageMetadata({ description: 'A'.repeat(161) });
    expect(errors.length).toBe(1);
    expect(errors[0]).toContain('50-160 characters');
  });

  it('accepts a description exactly 50 characters', () => {
    expect(validatePageMetadata({ description: 'A'.repeat(50) })).toEqual([]);
  });

  it('accepts a description exactly 160 characters', () => {
    expect(validatePageMetadata({ description: 'A'.repeat(160) })).toEqual([]);
  });

  it('skips validation when title/description are undefined', () => {
    expect(validatePageMetadata({})).toEqual([]);
  });

  it('reports multiple errors for both invalid title and description', () => {
    const errors = validatePageMetadata({ title: 'Hi', description: 'Bad' });
    expect(errors.length).toBe(2);
  });
});

describe('validateNavItem', () => {
  it('accepts a valid nav item with short label and / URL', () => {
    expect(validateNavItem({ label: 'Features', url: '/features/' })).toEqual([]);
  });

  it('accepts a valid nav item with https:// URL', () => {
    expect(validateNavItem({ label: 'Docs', url: 'https://docs.example.com' })).toEqual([]);
  });

  it('rejects a label longer than 30 characters', () => {
    const errors = validateNavItem({ label: 'A'.repeat(31), url: '/test/' });
    expect(errors.length).toBe(1);
    expect(errors[0]).toContain('≤30 characters');
  });

  it('accepts a label exactly 30 characters', () => {
    expect(validateNavItem({ label: 'A'.repeat(30), url: '/test/' })).toEqual([]);
  });

  it('rejects a missing label', () => {
    const errors = validateNavItem({ label: '', url: '/test/' });
    expect(errors.length).toBe(1);
    expect(errors[0]).toContain('≤30 characters');
  });

  it('rejects a URL that does not start with / or https://', () => {
    const errors = validateNavItem({ label: 'Test', url: 'relative/path' });
    expect(errors.length).toBe(1);
    expect(errors[0]).toContain('must start with / or https://');
  });

  it('rejects an http:// URL (must be / or https://)', () => {
    const errors = validateNavItem({ label: 'Test', url: 'http://example.com' });
    expect(errors.length).toBe(1);
    expect(errors[0]).toContain('must start with / or https://');
  });

  it('rejects a missing URL', () => {
    const errors = validateNavItem({ label: 'Test', url: '' });
    expect(errors.length).toBe(1);
    expect(errors[0]).toContain('must start with / or https://');
  });
});

describe('validateFeatureEntry', () => {
  it('accepts a valid feature entry', () => {
    const errors = validateFeatureEntry({
      title: 'Intent Compiler',
      description: 'Compiles natural language into typed IR.',
      category: 'core',
    });
    expect(errors).toEqual([]);
  });

  it('accepts all valid categories', () => {
    for (const category of ['core', 'developer', 'enterprise']) {
      expect(
        validateFeatureEntry({ title: 'Valid Title', description: 'Valid desc.', category })
      ).toEqual([]);
    }
  });

  it('rejects a title longer than 50 characters', () => {
    const errors = validateFeatureEntry({
      title: 'A'.repeat(51),
      description: 'Valid description.',
      category: 'core',
    });
    expect(errors.length).toBe(1);
    expect(errors[0]).toContain('≤50 characters');
  });

  it('accepts a title exactly 50 characters', () => {
    expect(
      validateFeatureEntry({ title: 'A'.repeat(50), description: 'OK', category: 'core' })
    ).toEqual([]);
  });

  it('rejects a missing title', () => {
    const errors = validateFeatureEntry({
      title: '',
      description: 'Valid.',
      category: 'core',
    });
    expect(errors.length).toBe(1);
    expect(errors[0]).toContain('≤50 characters');
  });

  it('rejects a description longer than 200 characters', () => {
    const errors = validateFeatureEntry({
      title: 'Valid',
      description: 'A'.repeat(201),
      category: 'core',
    });
    expect(errors.length).toBe(1);
    expect(errors[0]).toContain('≤200 characters');
  });

  it('accepts a description exactly 200 characters', () => {
    expect(
      validateFeatureEntry({ title: 'OK', description: 'A'.repeat(200), category: 'developer' })
    ).toEqual([]);
  });

  it('rejects an invalid category', () => {
    const errors = validateFeatureEntry({
      title: 'Valid',
      description: 'Valid.',
      category: 'invalid',
    });
    expect(errors.length).toBe(1);
    expect(errors[0]).toContain('must be one of');
  });

  it('reports multiple errors for multiple violations', () => {
    const errors = validateFeatureEntry({
      title: '',
      description: '',
      category: 'bad',
    });
    expect(errors.length).toBeGreaterThanOrEqual(2);
  });
});

describe('validatePricingPlans', () => {
  it('accepts valid plans with ≥3 features and at most one highlighted', () => {
    const plans = [
      { name: 'Free', features: ['a', 'b', 'c'], highlighted: false },
      { name: 'Pro', features: ['a', 'b', 'c', 'd'], highlighted: true },
    ];
    expect(validatePricingPlans(plans)).toEqual([]);
  });

  it('accepts plans with no highlighted plan', () => {
    const plans = [
      { name: 'A', features: ['a', 'b', 'c'], highlighted: false },
      { name: 'B', features: ['a', 'b', 'c'], highlighted: false },
    ];
    expect(validatePricingPlans(plans)).toEqual([]);
  });

  it('rejects plans with more than one highlighted', () => {
    const plans = [
      { name: 'A', features: ['a', 'b', 'c'], highlighted: true },
      { name: 'B', features: ['a', 'b', 'c'], highlighted: true },
    ];
    const errors = validatePricingPlans(plans);
    expect(errors.length).toBe(1);
    expect(errors[0]).toContain('Only one pricing plan may be highlighted');
  });

  it('rejects a plan with fewer than 3 features', () => {
    const plans = [
      { name: 'Basic', features: ['a', 'b'], highlighted: false },
    ];
    const errors = validatePricingPlans(plans);
    expect(errors.length).toBe(1);
    expect(errors[0]).toContain('≥3 features');
  });

  it('accepts a plan with exactly 3 features', () => {
    const plans = [
      { name: 'Min', features: ['a', 'b', 'c'], highlighted: false },
    ];
    expect(validatePricingPlans(plans)).toEqual([]);
  });

  it('rejects an empty array', () => {
    const errors = validatePricingPlans([]);
    expect(errors.length).toBe(1);
    expect(errors[0]).toContain('non-empty array');
  });

  it('handles plans with missing features array', () => {
    const plans = [{ name: 'Broken', highlighted: false }];
    const errors = validatePricingPlans(plans);
    expect(errors.length).toBe(1);
    expect(errors[0]).toContain('≥3 features');
  });
});

describe('validateBlogPost', () => {
  it('accepts a valid blog post', () => {
    const errors = validateBlogPost({
      date: '2024-06-01',
      tags: ['product'],
      excerpt: 'A short summary of the post.',
    });
    expect(errors).toEqual([]);
  });

  it('rejects a blog post with a future date', () => {
    const errors = validateBlogPost({
      date: '2099-12-31',
      tags: ['future'],
      excerpt: 'From the future.',
    });
    expect(errors.length).toBe(1);
    expect(errors[0]).toContain('must not be in the future');
  });

  it('accepts a blog post with today or a past date', () => {
    const yesterday = new Date();
    yesterday.setDate(yesterday.getDate() - 1);
    const errors = validateBlogPost({
      date: yesterday.toISOString().split('T')[0],
      tags: ['tech'],
      excerpt: 'Valid.',
    });
    expect(errors).toEqual([]);
  });

  it('rejects a blog post with no tags', () => {
    const errors = validateBlogPost({
      date: '2024-01-01',
      tags: [],
      excerpt: 'OK.',
    });
    expect(errors.length).toBe(1);
    expect(errors[0]).toContain('at least one tag');
  });

  it('rejects a blog post with missing tags field', () => {
    const errors = validateBlogPost({
      date: '2024-01-01',
      excerpt: 'OK.',
    });
    expect(errors.length).toBe(1);
    expect(errors[0]).toContain('at least one tag');
  });

  it('rejects a blog post with an excerpt longer than 200 characters', () => {
    const errors = validateBlogPost({
      date: '2024-01-01',
      tags: ['test'],
      excerpt: 'A'.repeat(201),
    });
    expect(errors.length).toBe(1);
    expect(errors[0]).toContain('≤200 characters');
  });

  it('accepts an excerpt exactly 200 characters', () => {
    const errors = validateBlogPost({
      date: '2024-01-01',
      tags: ['test'],
      excerpt: 'A'.repeat(200),
    });
    expect(errors).toEqual([]);
  });

  it('accepts a blog post without an excerpt (excerpt is optional)', () => {
    const errors = validateBlogPost({
      date: '2024-01-01',
      tags: ['test'],
    });
    expect(errors).toEqual([]);
  });
});
