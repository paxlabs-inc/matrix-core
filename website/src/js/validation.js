/**
 * Data Validation Utility Module
 *
 * Pure validation functions for build-time content validation.
 * Each function accepts data and returns an array of error strings.
 * An empty array means validation passed.
 *
 * Validates: Requirements 26.1, 26.2, 26.3, 26.4, 26.5, 16.6
 */

/**
 * Validates page metadata (title and description length constraints).
 * @param {Object} data - Page front matter data
 * @param {string} [data.title] - Page title (10-70 chars)
 * @param {string} [data.description] - Page description (50-160 chars)
 * @returns {string[]} Array of validation error messages
 */
export function validatePageMetadata(data) {
  const errors = [];

  if (data.title !== undefined && data.title !== null) {
    const titleLen = String(data.title).length;
    if (titleLen < 10 || titleLen > 70) {
      errors.push(
        `Title must be 10-70 characters, got ${titleLen}: "${data.title}"`
      );
    }
  }

  if (data.description !== undefined && data.description !== null) {
    const descLen = String(data.description).length;
    if (descLen < 50 || descLen > 160) {
      errors.push(
        `Description must be 50-160 characters, got ${descLen}`
      );
    }
  }

  return errors;
}

/**
 * Validates a navigation item (label length and URL format).
 * @param {Object} item - Navigation item
 * @param {string} item.label - Display text (≤30 chars)
 * @param {string} item.url - Link URL (must start with / or https://)
 * @returns {string[]} Array of validation error messages
 */
export function validateNavItem(item) {
  const errors = [];

  if (!item.label || String(item.label).length > 30) {
    errors.push(
      `Nav label must be present and ≤30 characters: "${item.label}"`
    );
  }

  if (!item.url || (!String(item.url).startsWith('/') && !String(item.url).startsWith('https://'))) {
    errors.push(
      `Nav URL must start with / or https://: "${item.url}"`
    );
  }

  return errors;
}

/**
 * Validates a feature entry (title, description, category).
 * @param {Object} feature - Feature data
 * @param {string} feature.title - Feature title (≤50 chars)
 * @param {string} feature.description - Feature description (≤200 chars)
 * @param {string} feature.category - Must be "core", "developer", or "enterprise"
 * @returns {string[]} Array of validation error messages
 */
export function validateFeatureEntry(feature) {
  const errors = [];
  const validCategories = ['core', 'developer', 'enterprise'];

  if (!feature.title || String(feature.title).length > 50) {
    errors.push(
      `Feature title must be present and ≤50 characters: "${feature.title}"`
    );
  }

  if (!feature.description || String(feature.description).length > 200) {
    errors.push(
      `Feature description must be present and ≤200 characters`
    );
  }

  if (!validCategories.includes(feature.category)) {
    errors.push(
      `Feature category must be one of: ${validCategories.join(', ')}. Got: "${feature.category}"`
    );
  }

  return errors;
}

/**
 * Validates a set of pricing plans (feature count and highlight constraints).
 * @param {Object[]} plans - Array of pricing plan objects
 * @param {string} plans[].name - Plan name
 * @param {string[]} plans[].features - List of features (≥3 required)
 * @param {boolean} [plans[].highlighted] - Whether plan is highlighted (only one allowed)
 * @returns {string[]} Array of validation error messages
 */
export function validatePricingPlans(plans) {
  const errors = [];

  if (!Array.isArray(plans) || plans.length === 0) {
    errors.push('Pricing plans must be a non-empty array');
    return errors;
  }

  const highlightedCount = plans.filter(p => p.highlighted).length;
  if (highlightedCount > 1) {
    errors.push(
      `Only one pricing plan may be highlighted, found ${highlightedCount}`
    );
  }

  for (const plan of plans) {
    if (!plan.features || !Array.isArray(plan.features) || plan.features.length < 3) {
      const count = Array.isArray(plan.features) ? plan.features.length : 0;
      errors.push(
        `Plan "${plan.name || 'unnamed'}" must have ≥3 features, got ${count}`
      );
    }
  }

  return errors;
}

/**
 * Validates blog post front matter data.
 * @param {Object} data - Blog post data
 * @param {string|Date} [data.date] - Publication date (must not be in the future)
 * @param {string[]} [data.tags] - Post tags (≥1 required)
 * @param {string} [data.excerpt] - Post excerpt (≤200 chars)
 * @returns {string[]} Array of validation error messages
 */
export function validateBlogPost(data) {
  const errors = [];

  if (data.date) {
    const postDate = new Date(data.date);
    const now = new Date();
    // Compare dates at day level to avoid timezone issues
    if (postDate.getTime() > now.getTime()) {
      errors.push(
        `Blog post date must not be in the future: ${data.date}`
      );
    }
  }

  if (!data.tags || !Array.isArray(data.tags) || data.tags.length === 0) {
    errors.push('Blog post must have at least one tag');
  }

  if (data.excerpt !== undefined && data.excerpt !== null) {
    const excerptLen = String(data.excerpt).length;
    if (excerptLen > 200) {
      errors.push(
        `Blog post excerpt must be ≤200 characters, got ${excerptLen}`
      );
    }
  }

  return errors;
}
