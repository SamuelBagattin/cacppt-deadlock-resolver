module.exports = {
  branches: [
    "main",
    {
      name: "*/*",
      prerelease: "${name.replace(/\\//g, '-')}",
    },
  ],
  plugins: [
    "@semantic-release/commit-analyzer",
    "@semantic-release/release-notes-generator",
    "@semantic-release/github",
    [
      "@semantic-release/exec",
      {
        publishCmd: 'echo "${nextRelease.version}" > .version',
      },
    ],
  ],
};
