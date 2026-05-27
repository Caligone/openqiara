// Package ota gère la mise à jour OTA du binaire openqiarad depuis
// les Releases GitHub.
//
// Flux :
//
//  1. CheckLatest() interroge l'API GitHub (cache 1h en RAM) et retourne
//     les infos sur la release la plus récente.
//  2. Install() télécharge le binaire ARM + SHA256SUMS, vérifie le
//     checksum, swappe le binaire en place et déclenche un restart.
//
// Le swap est conçu pour être robuste sur la cam Qiara où /data ne
// fait que 20 MB (souvent <5 MB libres) : on stage le download sur
// /media (~2.7 GB), on garde un backup de l'ancien binaire sur /media
// (rollback manuel possible), puis on remplace /data/openqiarad.
//
// Hors-scope :
//   - Pas de rollback automatique au boot (le watchdog boot.sh
//     redémarre le binaire mais ne sait pas détecter un crash en boucle).
//     Si le nouveau binaire panic au boot, rollback SSH manuel via
//     /media/openqiarad.old.
//   - Pas de signature cryptographique au-delà du SHA256 servi par
//     GitHub en HTTPS (suffisant pour alpha).
package ota

// Repo détermine quel repo GitHub on interroge pour les releases.
// Override en test via Client.repo.
const defaultRepo = "Caligone/openqiara"

// AssetName est le fichier de la release qu'on télécharge sur la cam
// (binaire ARMv7). Doit matcher le nom produit par .github/workflows/release.yml.
const AssetName = "openqiarad-linux-arm7"

// BootScriptName est le script shell de boot qu'on déploie en /data/boot.sh.
// Updaté en même temps que le binaire pour que les flags de lancement
// (et autres ajustements système au boot) restent en phase avec le binaire.
const BootScriptName = "camera_boot.sh"

// ChecksumsName est le fichier qui contient les SHA256 des assets.
const ChecksumsName = "SHA256SUMS"

// Release représente les infos minimales qu'on extrait d'une release
// GitHub. Les autres champs (auteur, asset URLs détaillées, ...) sont
// ignorés.
type Release struct {
	TagName     string `json:"tag_name"`     // ex "v0.1.0-alpha.2"
	Name        string `json:"name"`         // titre affichable
	Body        string `json:"body"`         // notes markdown
	PublishedAt string `json:"published_at"` // ISO 8601
	HTMLURL     string `json:"html_url"`     // lien page release
	Prerelease  bool   `json:"prerelease"`
}

// CheckResult est ce qu'on retourne à l'UI : version courante + dernière
// release disponible + diff binaire.
type CheckResult struct {
	Current      string  `json:"current"`            // version qui tourne actuellement
	Latest       Release `json:"latest"`             // dernière release publiée
	UpdateNeeded bool    `json:"update_needed"`      // current != latest.TagName et current != "dev"
	CheckedAt    string  `json:"checked_at"`         // ISO 8601 de quand on a checké
}
