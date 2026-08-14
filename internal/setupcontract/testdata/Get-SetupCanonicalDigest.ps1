param(
    [Parameter(Mandatory = $true)][string]$Path,
    [Parameter(Mandatory = $true)][string]$OutputPath
)

$ErrorActionPreference = "Stop"
Add-Type -AssemblyName System.Web.Extensions
Add-Type -TypeDefinition @'
using System;
using System.Collections;
using System.Collections.Generic;

public sealed class UnicodeCodePointComparer : IComparer<string>
{
    public static readonly UnicodeCodePointComparer Instance = new UnicodeCodePointComparer();
	public static string[] SortedKeys(IDictionary values)
	{
		string[] keys = new string[values.Keys.Count];
		int index = 0;
		foreach (object key in values.Keys) keys[index++] = Convert.ToString(key);
		Array.Sort(keys, Instance);
		return keys;
	}

    public int Compare(string left, string right)
    {
        int leftIndex = 0, rightIndex = 0;
        while (leftIndex < left.Length && rightIndex < right.Length)
        {
            int leftPoint = Char.ConvertToUtf32(left, leftIndex);
            int rightPoint = Char.ConvertToUtf32(right, rightIndex);
            if (leftPoint != rightPoint) return leftPoint < rightPoint ? -1 : 1;
            leftIndex += Char.IsSurrogatePair(left, leftIndex) ? 2 : 1;
            rightIndex += Char.IsSurrogatePair(right, rightIndex) ? 2 : 1;
        }
        return (left.Length - leftIndex).CompareTo(right.Length - rightIndex);
    }
}
'@

function Write-JsonString([System.Text.StringBuilder]$Builder, [string]$Value) {
    [void]$Builder.Append('"')
    foreach ($character in $Value.ToCharArray()) {
        $number = [int]$character
		$escaped = $true
        switch ($number) {
			8 { [void]$Builder.Append('\b') }
			9 { [void]$Builder.Append('\t') }
			10 { [void]$Builder.Append('\n') }
			12 { [void]$Builder.Append('\f') }
			13 { [void]$Builder.Append('\r') }
			34 { [void]$Builder.Append('\"') }
			92 { [void]$Builder.Append('\\') }
			default { $escaped = $false }
        }
		if ($escaped) { continue }
        if ($number -lt 32) {
            [void]$Builder.Append(('\u{0:x4}' -f $number))
        } else {
            [void]$Builder.Append($character)
        }
    }
    [void]$Builder.Append('"')
}

function Write-CanonicalValue([System.Text.StringBuilder]$Builder, $Value) {
    if ($null -eq $Value) { [void]$Builder.Append('null'); return }
    if ($Value -is [string]) { Write-JsonString $Builder $Value; return }
    if ($Value -is [bool]) { [void]$Builder.Append($(if ($Value) { 'true' } else { 'false' })); return }
    if ($Value -is [byte] -or $Value -is [sbyte] -or $Value -is [int16] -or $Value -is [uint16] -or $Value -is [int32] -or $Value -is [uint32] -or $Value -is [int64] -or $Value -is [uint64]) {
        [void]$Builder.Append($Value.ToString([Globalization.CultureInfo]::InvariantCulture)); return
    }
    if ($Value -is [System.Collections.IDictionary]) {
		[string[]]$keys = [UnicodeCodePointComparer]::SortedKeys($Value)
        [void]$Builder.Append('{')
        for ($index = 0; $index -lt $keys.Count; $index++) {
            if ($index -gt 0) { [void]$Builder.Append(',') }
            Write-JsonString $Builder $keys[$index]
            [void]$Builder.Append(':')
            Write-CanonicalValue $Builder $Value[$keys[$index]]
        }
        [void]$Builder.Append('}')
        return
    }
    if ($Value -is [System.Collections.IEnumerable]) {
        [void]$Builder.Append('[')
        $index = 0
        foreach ($item in $Value) {
            if ($index -gt 0) { [void]$Builder.Append(',') }
            Write-CanonicalValue $Builder $item
            $index++
        }
        [void]$Builder.Append(']')
        return
    }
    throw "Floating-point or unsupported JSON value type $($Value.GetType().FullName)"
}

$utf8 = New-Object System.Text.UTF8Encoding($false, $true)
$raw = [IO.File]::ReadAllText((Resolve-Path -LiteralPath $Path), $utf8)
$serializer = New-Object System.Web.Script.Serialization.JavaScriptSerializer
$value = $serializer.DeserializeObject($raw)
$builder = New-Object System.Text.StringBuilder
Write-CanonicalValue $builder $value
$canonical = $builder.ToString()
[IO.File]::WriteAllText($OutputPath, $canonical, $utf8)
$sha = [Security.Cryptography.SHA256]::Create()
try {
    $digest = ([BitConverter]::ToString($sha.ComputeHash($utf8.GetBytes($canonical)))).Replace('-', '').ToLowerInvariant()
} finally {
    $sha.Dispose()
}
Write-Output $digest
