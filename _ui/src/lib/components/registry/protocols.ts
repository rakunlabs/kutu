// Protocol catalog — one row per registry type kutu ships with.
//
// Everything type-specific that the generic registry UI needs
// (labels, default paths, upstream URLs, client setup snippets) lives
// here, so adding a protocol is a single entry instead of a dozen
// switch arms across Registries.svelte and PackageDetailPanel.svelte.

import type { RegistryType } from './types';

export type ProtocolInfo = {
  type: RegistryType;
  /** Human label for the type select (plural noun). */
  label: string;
  /** Short name for badges. */
  short: string;
  /** Default local base path / remote cache path. */
  basePath: string;
  cachePath: string;
  /** Default upstream for a new remote repo ('' = none). */
  upstream: string;
  /** Hint shown under the mutable TTL field. */
  ttlHint: string;
  /** Uses the protocol-neutral /entries + generic detail endpoints. */
  generic: boolean;
  /** Local kind accepts a signing key (apt/rpm/alpine). */
  signing?: 'pgp' | 'rsa';
  /** Remote kind honours prefix-routed upstreams. */
  prefixUpstreams?: boolean;
  /** Client setup snippet. {base} = endpoint URL, {host}, {repo}. */
  setup: string;
  /** Snippet for one package (detail panel). {name}, {version}. */
  install?: string;
};

const P = (p: ProtocolInfo) => p;

export const PROTOCOLS: ProtocolInfo[] = [
  P({
    type: 'go', label: 'Go modules', short: 'Go', basePath: 'go/', cachePath: 'go-cache/',
    upstream: 'https://proxy.golang.org', generic: false, prefixUpstreams: true,
    ttlHint: 'Caches @v/list and @latest; .info/.mod/.zip are immutable.',
    setup: 'export GOPROXY={base},direct\nexport GONOSUMDB=*   # for private modules',
  }),
  P({
    type: 'npm', label: 'NPM packages', short: 'npm', basePath: 'npm/', cachePath: 'npm-cache/',
    upstream: 'https://registry.npmjs.org', generic: false, prefixUpstreams: true,
    ttlHint: 'Caches packuments (versions + dist-tags); tarballs are immutable.',
    setup: 'npm config set registry {base}/\nnpm config set //{hostpath}/:_authToken <token>',
  }),
  P({
    type: 'docker', label: 'Docker / OCI', short: 'OCI', basePath: 'docker/', cachePath: 'docker-cache/',
    upstream: 'https://registry-1.docker.io', generic: false,
    ttlHint: 'Applies only to floating tags; digests and pinned tags are immutable.',
    setup: 'docker login {host}\ndocker pull {host}/IMAGE:TAG   # via a dedicated registry listener',
  }),
  P({
    type: 'helm', label: 'Helm charts', short: 'Helm', basePath: 'charts/', cachePath: 'charts-cache/',
    upstream: 'https://charts.bitnami.com/bitnami', generic: false,
    ttlHint: 'Caches index.yaml; chart archives are immutable.',
    setup: 'helm repo add kutu {base} --username x --password <token>',
  }),
  P({
    type: 'maven', label: 'Maven artifacts', short: 'Maven', basePath: 'maven/', cachePath: 'maven-cache/',
    upstream: 'https://repo1.maven.org/maven2', generic: false, prefixUpstreams: true,
    ttlHint: 'Caches maven-metadata.xml; versioned artifacts are immutable.',
    setup: '<repository>\n  <id>kutu</id>\n  <url>{base}</url>\n</repository>',
  }),
  P({
    type: 'pypi', label: 'PyPI packages', short: 'PyPI', basePath: 'pypi/', cachePath: 'pypi-cache/',
    upstream: 'https://pypi.org', generic: false,
    ttlHint: 'Caches simple index pages; distribution files are immutable.',
    setup: 'pip install --index-url {base}/simple/ PACKAGE\ntwine upload --repository-url {base}/ dist/*',
  }),
  P({
    type: 'cargo', label: 'Cargo crates', short: 'Cargo', basePath: 'cargo/', cachePath: 'cargo-cache/',
    upstream: 'https://index.crates.io', generic: false,
    ttlHint: 'Caches sparse-index files; .crate archives are immutable.',
    setup: '# ~/.cargo/config.toml\n[registries.kutu]\nindex = "sparse+{base}/"\n\ncargo login --registry kutu <token>',
  }),
  P({
    type: 'generic', label: 'Generic files', short: 'Generic', basePath: 'generic/', cachePath: 'generic-cache/',
    upstream: '', generic: true,
    ttlHint: 'Caches package / version listings; files are immutable.',
    setup: 'curl -u x:<token> -T app.tar.gz {base}/PACKAGE/VERSION/app.tar.gz\ncurl -u x:<token> -O {base}/PACKAGE/VERSION/app.tar.gz',
    install: 'curl -u x:<token> -O {base}/{name}/{version}/FILE',
  }),
  P({
    type: 'nuget', label: 'NuGet packages', short: 'NuGet', basePath: 'nuget/', cachePath: 'nuget-cache/',
    upstream: 'https://api.nuget.org/v3/index.json', generic: true,
    ttlHint: 'Caches version indexes and registrations; .nupkg files are immutable.',
    setup: 'dotnet nuget add source {base}/v3/index.json -n kutu -u x -p <token> --store-password-in-clear-text\ndotnet nuget push pkg.nupkg -s kutu -k <token>',
    install: 'dotnet add package {name} --version {version} --source kutu',
  }),
  P({
    type: 'rubygems', label: 'RubyGems', short: 'Gems', basePath: 'gems/', cachePath: 'gems-cache/',
    upstream: 'https://rubygems.org', generic: true,
    ttlHint: 'Caches the compact index (/versions, /info); .gem files are immutable.',
    setup: '# Gemfile\nsource "{base}"\n\nbundle config set --global {host} x:<token>\ngem push --host {base} pkg.gem',
    install: 'gem "{name}", "{version}", source: "{base}"',
  }),
  P({
    type: 'composer', label: 'Composer (PHP)', short: 'Composer', basePath: 'composer/', cachePath: 'composer-cache/',
    upstream: 'https://repo.packagist.org', generic: true,
    ttlHint: 'Caches p2 metadata; dist zips are immutable.',
    setup: '"repositories": [{"type": "composer", "url": "{base}"}]\n\ncomposer config --global http-basic.{host} x <token>\ncurl -u x:<token> -T pkg.zip "{base}/api/upload?version=1.0.0"',
    install: 'composer require {name}:{version}',
  }),
  P({
    type: 'terraform', label: 'Terraform / OpenTofu', short: 'Terraform', basePath: 'terraform/', cachePath: 'terraform-cache/',
    upstream: 'https://registry.terraform.io', generic: true,
    ttlHint: 'Caches version lists; module archives and provider zips are immutable.',
    setup: '# needs a dedicated registry listener (host root)\n# ~/.terraformrc\ncredentials "{host}" { token = "<token>" }\n\nmodule "x" { source = "{host}/NS/NAME/SYSTEM" }',
  }),
  P({
    type: 'pub', label: 'Dart / Flutter (pub)', short: 'pub', basePath: 'pub/', cachePath: 'pub-cache/',
    upstream: 'https://pub.dev', generic: true,
    ttlHint: 'Caches package metadata; archives are immutable.',
    setup: 'dart pub token add {base}\n# pubspec.yaml\npublish_to: {base}',
    install: 'dependencies:\n  {name}:\n    hosted: {base}\n    version: {version}',
  }),
  P({
    type: 'swift', label: 'Swift packages', short: 'Swift', basePath: 'swift/', cachePath: 'swift-cache/',
    upstream: '', generic: true,
    ttlHint: 'Caches release lists; source archives are immutable.',
    setup: 'swift package-registry set {base}\nswift package-registry login {base} --token <token>\nswift package-registry publish SCOPE.NAME 1.0.0',
  }),
  P({
    type: 'apt', label: 'APT (Debian)', short: 'APT', basePath: 'apt/', cachePath: 'apt-cache/',
    upstream: 'http://deb.debian.org/debian', generic: true, signing: 'pgp',
    ttlHint: 'Caches Release / Packages files; pool/ .deb files are immutable.',
    setup: 'curl -fsSL {base}/public.key | sudo gpg --dearmor -o /etc/apt/keyrings/kutu.gpg\necho "deb [signed-by=/etc/apt/keyrings/kutu.gpg] {base} stable main" | sudo tee /etc/apt/sources.list.d/kutu.list\ncurl -u x:<token> -T pkg.deb {base}/upload/stable/main',
    install: 'sudo apt install {name}={version}',
  }),
  P({
    type: 'rpm', label: 'RPM (YUM / DNF)', short: 'RPM', basePath: 'rpm/', cachePath: 'rpm-cache/',
    upstream: '', generic: true, signing: 'pgp',
    ttlHint: 'Caches repomd.xml; checksum-named repodata and packages are immutable.',
    setup: 'sudo curl -u x:<token> -o /etc/yum.repos.d/kutu.repo {base}/config.repo\ncurl -u x:<token> -T pkg.rpm {base}/upload',
    install: 'sudo dnf install {name}-{version}',
  }),
  P({
    type: 'alpine', label: 'Alpine (apk)', short: 'apk', basePath: 'alpine/', cachePath: 'alpine-cache/',
    upstream: 'https://dl-cdn.alpinelinux.org/alpine', generic: true, signing: 'rsa',
    ttlHint: 'Caches APKINDEX.tar.gz; .apk files are immutable.',
    setup: 'wget -O /etc/apk/keys/kutu.rsa.pub {base}/keys/kutu.rsa.pub\necho "{base}/v3.20/main" >> /etc/apk/repositories\ncurl -u x:<token> -T pkg.apk {base}/upload/v3.20/main',
    install: 'apk add {name}={version}',
  }),
  P({
    type: 'conda', label: 'Conda channel', short: 'Conda', basePath: 'conda/', cachePath: 'conda-cache/',
    upstream: 'https://conda.anaconda.org/conda-forge', generic: true,
    ttlHint: 'Caches repodata.json; package archives are immutable.',
    setup: 'conda config --add channels {base}\ncurl -u x:<token> -T pkg.conda {base}/upload/linux-64',
    install: 'conda install -c {base} {name}={version}',
  }),
  P({
    type: 'huggingface', label: 'Hugging Face Hub', short: 'HF', basePath: 'hf/', cachePath: 'hf-cache/',
    upstream: 'https://huggingface.co', generic: true,
    ttlHint: 'Caches branch → commit resolution; files by commit are immutable.',
    setup: 'export HF_ENDPOINT={base}\nexport HF_TOKEN=<token>\nhuggingface-cli download ORG/MODEL',
    install: 'HF_ENDPOINT={base} huggingface-cli download {name} --revision {version}',
  }),
  P({
    type: 'conan', label: 'Conan (C/C++)', short: 'Conan', basePath: 'conan/', cachePath: 'conan-cache/',
    upstream: 'https://center2.conan.io', generic: true,
    ttlHint: 'Caches revision lists and search; files under a revision are immutable.',
    setup: 'conan remote add kutu {base}\nconan remote login kutu x -p <token>\nconan upload "pkg/*" -r kutu',
  }),
  P({
    type: 'cran', label: 'CRAN (R)', short: 'CRAN', basePath: 'cran/', cachePath: 'cran-cache/',
    upstream: 'https://cloud.r-project.org', generic: true,
    ttlHint: 'Caches PACKAGES indexes; package archives are immutable.',
    setup: 'options(repos = c(kutu = "{base}"))\ncurl -u x:<token> -T pkg_1.0.tar.gz {base}/upload',
    install: 'install.packages("{name}", repos = "{base}")',
  }),
  P({
    type: 'vagrant', label: 'Vagrant boxes', short: 'Vagrant', basePath: 'vagrant/', cachePath: 'vagrant-cache/',
    upstream: 'https://vagrantcloud.com', generic: true,
    ttlHint: 'Caches box metadata; .box files are immutable.',
    setup: 'vagrant box add {base}/ORG/BOX\ncurl -u x:<token> -T dev.box {base}/ORG/BOX/1.0.0/virtualbox/amd64',
  }),
  P({
    type: 'ansible', label: 'Ansible Galaxy', short: 'Galaxy', basePath: 'ansible/', cachePath: 'ansible-cache/',
    upstream: 'https://galaxy.ansible.com', generic: true,
    ttlHint: 'Caches version lists; collection tarballs are immutable.',
    setup: '# ansible.cfg\n[galaxy]\nserver_list = kutu\n\n[galaxy_server.kutu]\nurl={base}/api/\ntoken=<token>',
    install: 'ansible-galaxy collection install {name}:{version}',
  }),
  P({
    type: 'puppet', label: 'Puppet Forge', short: 'Puppet', basePath: 'puppet/', cachePath: 'puppet-cache/',
    upstream: 'https://forgeapi.puppet.com', generic: true,
    ttlHint: 'Caches module/release listings; release tarballs are immutable.',
    setup: 'puppet module install OWNER-NAME --module_repository {base}',
  }),
  P({
    type: 'chef', label: 'Chef Supermarket', short: 'Chef', basePath: 'chef/', cachePath: 'chef-cache/',
    upstream: 'https://supermarket.chef.io', generic: true,
    ttlHint: 'Caches /universe; cookbook tarballs are immutable.',
    setup: '# Berksfile\nsource "{base}"',
  }),
  P({
    type: 'cocoapods', label: 'CocoaPods', short: 'Pods', basePath: 'cocoapods/', cachePath: 'cocoapods-cache/',
    upstream: 'https://cdn.cocoapods.org', generic: true,
    ttlHint: 'Caches shard index files; podspecs are immutable.',
    setup: "# Podfile\nsource '{base}/'",
  }),
  P({
    type: 'bower', label: 'Bower', short: 'Bower', basePath: 'bower/', cachePath: 'bower-cache/',
    upstream: 'https://registry.bower.io', generic: true,
    ttlHint: 'Caches registry lookups.',
    setup: '# .bowerrc\n{"registry": "{base}"}',
  }),
  P({
    type: 'gitlfs', label: 'Git LFS', short: 'LFS', basePath: 'lfs/', cachePath: 'lfs-cache/',
    upstream: '', generic: true,
    ttlHint: 'LFS objects are content-addressed and immutable.',
    setup: 'git config lfs.url {base}\n# credential helper: user x, password <token>',
  }),
  P({
    type: 'p2', label: 'Eclipse p2', short: 'p2', basePath: 'p2/', cachePath: 'p2-cache/',
    upstream: 'https://download.eclipse.org/releases/latest', generic: true,
    ttlHint: 'Caches content/artifacts metadata; plugin and feature jars are immutable.',
    setup: '# Eclipse → Help → Install New Software → Add…\n{base}/\ncurl -u x:<token> -T site.zip {base}/upload/SITE',
  }),
];

const BY_TYPE = new Map(PROTOCOLS.map((p) => [p.type, p]));

export function protocol(type: RegistryType): ProtocolInfo {
  return BY_TYPE.get(type) ?? PROTOCOLS[0];
}

export function fillSnippet(tpl: string, vars: Record<string, string>): string {
  return tpl.replace(/\{(\w+)\}/g, (m, k) => (k in vars ? vars[k] : m));
}

/** Variables for setup snippets derived from an endpoint URL. */
export function snippetVars(endpoint: string, repo: string): Record<string, string> {
  let host = '';
  let hostpath = '';
  try {
    const u = new URL(endpoint);
    host = u.host;
    hostpath = u.host + u.pathname.replace(/\/$/, '');
  } catch {
    /* relative endpoint */
  }
  return { base: endpoint, host, hostpath, repo };
}
