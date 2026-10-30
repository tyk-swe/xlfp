package numprint

import (
	"fmt"
	"math/rand"
)

// Challenge is one number-writing prompt.
type Challenge struct {
	ID            string `json:"id"`
	Language      string `json:"language"`
	ExpectedCount int    `json:"expected_count"`
	Prompt        string `json:"prompt"`
}

// challengeTemplates are xlfp's own prompt wordings. Each asks for
// {n} whole numbers between 1 and 355, each picked by immediate gut
// feeling, forbids tools/code and pattern-made sequences, and asks
// for numbers only. Wording variety keeps one fixed phrasing from
// dominating the fingerprint.
var challengeTemplates = map[string][]string{
	"en": {
		"Write exactly %d whole numbers, every one between 1 and 355. Pick each number on its own, by your very first gut feeling, one at a time. Do not use tools or code, and do not work anything out. Avoid counting up or down, runs that only rise or only fall, evenly spaced steps, repeating cycles, or any other pattern you could describe with a rule. Reply with the numbers only, separated by spaces.",
		"Give me %d integers from 1 to 355. Choose every single one separately, whatever pops into your head first. No tools, no code, no calculations. Stay away from sequences, steady climbs or drops, regular gaps, loops, or anything rule-made. Output nothing but the numbers, with spaces between them.",
		"I need %d numbers. Each must be a whole number from 1 to 355, chosen purely by first instinct, independently of the ones before it. Please do not use any tool or code, and avoid counting, arithmetic patterns, repeated cycles, and long rising or falling stretches. Just the numbers, space-separated, nothing else.",
	},
	"zh": {
		"请写出恰好 %d 个整数，每个都在 1 到 355 之间。每个数字都要单独决定，凭第一直觉，一个一个来。不要使用工具或代码，也不要做任何计算。避免从小到大或从大到小地数数，避免一直上升或一直下降，避免等差、循环重复或任何可以用规则描述的模式。只回复数字，用空格分隔。",
		"给我 %d 个 1 到 355 之间的整数。每一个都分开选，想到什么就写什么。不要用工具，不要写代码，不要计算。避开数列式的排列、持续上升或下降、固定间隔、循环，以及其它任何规律性的排法。只输出数字，中间用空格隔开。",
		"我需要 %d 个数字，每个都是 1 到 355 之间的整数，完全凭第一感觉独立选出。请不要使用工具或代码，并避免数数、等差规律、重复循环和长时间只升不降或只降不升。只写数字，用空格分隔，其它什么都不要。",
	},
	"ja": {
		"1から355までの整数をちょうど%d個書いてください。数字は一つずつ、最初の直感だけで独立に選んでください。ツールやコードは使わず、計算もしないでください。数え上げ・数え下げ、上がり続ける・下がり続ける並び、等間隔、繰り返しの循環、その他ルールで説明できるパターンは避けてください。数字だけをスペース区切りで答えてください。",
		"%d個の整数（1から355の範囲）をください。どれも別々に、最初に思い浮かんだものを選んでください。ツール・コード・計算は禁止です。数列のような並び、一定の上昇や下降、規則的な間隔、ループなど、規則で作られたものは避けてください。スペースで区切った数字だけを出力してください。",
		"%d個の数字が必要です。各数字は1から355の整数で、前の数字とは無関係に、純粋に最初の感覚で選んでください。ツールやコードを使わず、数えること、等差のパターン、繰り返しのサイクル、長い上昇・下降を避けてください。数字だけをスペース区切りで書いてください。",
	},
	"ko": {
		"1부터 355 사이의 정수를 정확히 %d개 써 주세요. 각 숫자는 하나씩, 첫 직감만으로 따로따로 고르세요. 도구나 코드를 사용하지 말고 계산도 하지 마세요. 순서대로 세기, 계속 오르거나 내리기만 하는 배열, 일정한 간격, 반복되는 순환, 그 밖에 규칙으로 설명할 수 있는 패턴은 피하세요. 숫자만 공백으로 구분해 답하세요.",
		"1에서 355 사이의 정수 %d개를 주세요. 하나하나 따로, 가장 먼저 떠오르는 것을 고르세요. 도구, 코드, 계산은 안 됩니다. 수열 같은 배열, 꾸준한 상승이나 하락, 규칙적인 간격, 반복 등 규칙으로 만든 것은 피하세요. 공백으로 구분한 숫자만 출력하세요.",
		"숫자 %d개가 필요합니다. 각각 1부터 355 사이의 정수여야 하고, 앞의 숫자와 관계없이 순전히 첫 느낌으로 골라야 합니다. 도구나 코드를 쓰지 말고, 세기, 등차 패턴, 반복 순환, 긴 상승이나 하락 구간을 피하세요. 숫자만 공백으로 구분해 쓰세요.",
	},
	"fr": {
		"Écris exactement %d nombres entiers, chacun entre 1 et 355. Choisis chaque nombre séparément, au tout premier instinct, un par un. N'utilise ni outil ni code, et ne calcule rien. Évite de compter dans l'ordre croissant ou décroissant, les suites qui ne font que monter ou descendre, les écarts réguliers, les cycles répétés et tout motif descriptible par une règle. Réponds uniquement avec les nombres, séparés par des espaces.",
		"Donne-moi %d entiers entre 1 et 355. Choisis chacun isolément, le premier qui te vient à l'esprit. Aucun outil, aucun code, aucun calcul. Évite les arrangements en suite, les montées ou descentes continues, les intervalles fixes, les boucles et tout ce qui suit une règle. Affiche seulement les nombres, séparés par des espaces.",
		"Il me faut %d nombres. Chacun doit être un entier de 1 à 355, choisi purement au premier ressenti, indépendamment des précédents. Merci de n'utiliser ni outil ni code, et d'éviter le comptage, les motifs arithmétiques, les cycles répétés et les longues montées ou descentes. Rien que les nombres, séparés par des espaces.",
	},
}

var challengeLanguages = []string{"en", "zh", "ja", "ko", "fr"}

// GenerateChallenges draws count challenges (1–3 for a detection
// round): requested counts in 292..332 without repetition, each in
// an independently drawn language and template.
func GenerateChallenges(r *rand.Rand, count int) []Challenge {
	if count < 1 {
		count = 1
	}
	pool := make([]int, 0, 41)
	for n := 292; n <= 332; n++ {
		pool = append(pool, n)
	}
	r.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	out := make([]Challenge, 0, count)
	for i := 0; i < count; i++ {
		n := pool[i%len(pool)]
		lang := challengeLanguages[r.Intn(len(challengeLanguages))]
		tmpls := challengeTemplates[lang]
		tmpl := tmpls[r.Intn(len(tmpls))]
		out = append(out, Challenge{
			ID:            fmt.Sprintf("c%d", i+1),
			Language:      lang,
			ExpectedCount: n,
			Prompt:        fmt.Sprintf(tmpl, n),
		})
	}
	return out
}
