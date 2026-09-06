const base = require("./.textlintrc.json");

module.exports = {
  rules: {
    ...base.rules,
    "preset-ja-technical-writing": {
      ...base.rules["preset-ja-technical-writing"],
      "sentence-length": false,
    },
  },
};
