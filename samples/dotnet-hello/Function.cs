using System.Text.Json;
using System.Text.Json.Serialization;
using Amazon.Lambda.APIGatewayEvents;
using Amazon.Lambda.Core;
using Amazon.Lambda.RuntimeSupport;
using Amazon.Lambda.Serialization.SystemTextJson;

namespace DotnetHello;

public static class Function
{
    private static readonly string Build = BuildInfo.Id;

    // Input is a raw JSON element so both APIGW v2 requests and EventBridge scheduled events work.
    private static APIGatewayHttpApiV2ProxyResponse Handler(JsonElement input, ILambdaContext context)
    {
        if (input.ValueKind == JsonValueKind.Object && input.TryGetProperty("detail-type", out _))
        {
            Console.WriteLine("scheduled tick");
            return Respond(new Body("dotnet", null, null, Build));
        }

        string? path = input.ValueKind == JsonValueKind.Object && input.TryGetProperty("rawPath", out var p)
            ? p.GetString() : null;

        if (input.ValueKind == JsonValueKind.Object
            && input.TryGetProperty("queryStringParameters", out var q) && q.ValueKind == JsonValueKind.Object
            && q.TryGetProperty("sleep", out var s) && TryParseDuration(s.GetString(), out var d))
        {
            Thread.Sleep(d);
        }

        return Respond(new Body("dotnet", path, Environment.GetEnvironmentVariable("AWS_LAMBDA_FUNCTION_VERSION"), Build));
    }

    // Accepts Go-style durations used by the go sample: "500ms", "2s", "1m".
    private static bool TryParseDuration(string? v, out TimeSpan d)
    {
        d = default;
        if (string.IsNullOrEmpty(v)) return false;
        if (v.EndsWith("ms") && double.TryParse(v[..^2], out var ms)) { d = TimeSpan.FromMilliseconds(ms); return true; }
        if (v.EndsWith('s') && double.TryParse(v[..^1], out var sec)) { d = TimeSpan.FromSeconds(sec); return true; }
        if (v.EndsWith('m') && double.TryParse(v[..^1], out var min)) { d = TimeSpan.FromMinutes(min); return true; }
        return false;
    }

    private static APIGatewayHttpApiV2ProxyResponse Respond(Body b) => new()
    {
        StatusCode = 200,
        Headers = new Dictionary<string, string> { ["content-type"] = "application/json" },
        Body = JsonSerializer.Serialize(b, BodyContext.Default.Body),
    };

    private static async Task Main()
    {
        Func<JsonElement, ILambdaContext, APIGatewayHttpApiV2ProxyResponse> handler = Handler;
        await LambdaBootstrapBuilder.Create(handler, new SourceGeneratorLambdaJsonSerializer<LambdaContext>())
            .Build()
            .RunAsync();
    }
}

public record Body(
    [property: JsonPropertyName("lang")] string Lang,
    [property: JsonPropertyName("path")] string? Path,
    [property: JsonPropertyName("version")] string? Version,
    [property: JsonPropertyName("build")] string Build);

[JsonSerializable(typeof(Body))]
public partial class BodyContext : JsonSerializerContext { }

[JsonSerializable(typeof(JsonElement))]
[JsonSerializable(typeof(APIGatewayHttpApiV2ProxyResponse))]
public partial class LambdaContext : JsonSerializerContext { }

